# Seera — a routing engine in front of the answer path

## Context

Today every message takes one path: retrieve → rerank → generate → validate. Two
consequences, both visible in use.

"Hello" was answered with _"I could not find anything in the indexed Ethiopian
legal sources"_. The stopgap for that —
[internal/rag/smalltalk.go](../../seera-backend-services/internal/rag/smalltalk.go)
— is a hardcoded phrase list. It cannot recognise "hey mate", "can you help me
understand something", an Amharic greeting outside the handful I was confident
about, or "explain that more simply". It is a lookup table pretending to be
understanding, and it is the wrong mechanism.

And an answer ends where the retrieval ended. The model quotes the provisions it
was handed and stops. A follow-up re-retrieves from scratch, so "explain the
second one" has to find that provision again from a pronoun, and the sources
already on the reader's screen are not available to the model as text.

This plan replaces the phrase list with a **model-driven router**, adds a
**constrained general engine** for everything that is not a legal question, and
gives the legal engine **memory of the citations already in the conversation** so
a thread can be discussed rather than restarted. It also deepens the answers
themselves — explanation and reasoning over the retrieved provisions, and a
second retrieval pass when the first one left a gap.

Two decisions taken with the user:

- The general engine **may explain general concepts**, always labelled as general
  knowledge and never as Ethiopian law, always offering to search the corpus.
- **All phases** are in scope for the first implementation pass.

One requirement this satisfies that already existed: **SRS FR-QA-002, "the system
shall classify and normalize the user query"** — never implemented.

## What exists today

| Piece               | Where                                          | State                                                                                 |
| ------------------- | ---------------------------------------------- | ------------------------------------------------------------------------------------- |
| Answer pipeline     | `internal/rag/service.go` `Answer()`           | Retrieval → rerank → context → generate → marker validation                           |
| Follow-up rewrite   | `internal/rag/followup.go` `retrievalQuery()`  | One LLM call, 5s cap, falls back to joined query                                      |
| Intent gate         | `internal/rag/smalltalk.go`                    | Phrase list. **To be deleted**                                                        |
| Grounding           | `internal/rag/grounding.go` `MarkerFilter`     | Validates every `[S1]` against this request's context as it streams                   |
| Context builder     | `internal/rag/context.go` `BuildContext`       | Numbers sources, budgets tokens, keeps rank order                                     |
| Retrieval           | `internal/search/service.go`                   | Vector ∪ keyword ∪ trigram ∪ **exact article** (`articleLookup`, private), RRF fusion |
| Citation resolution | `internal/citations/repository.go` `Resolve()` | chunk ids → full text + version metadata. Errors if any id is missing                 |
| LLM boundary        | `internal/ai/llm.go`                           | `Stream` only. Gemini is the only implementation                                      |
| HTTP surface        | `internal/qa/{dto,handler}.go`                 | Validates, streams SSE, owns the wire contract                                        |
| Evaluation          | `tests/evaluation/.gitkeep`                    | **Empty.** The harness the plan doc specifies was never built                         |

## Target architecture

```
  question + history + citations already in the thread
                        │
                        ▼
              ┌───────────────────┐
              │   internal/router │   one structured LLM call:
              │  classify + rewrite│   intent, standalone query, depth,
              └─────────┬─────────┘   reuse-prior-sources, language
                        │
        ┌───────────────┼────────────────────────┐
        ▼               ▼                        ▼
 internal/chat    internal/rag (legal)     internal/rag (discussion)
 no corpus,       retrieve → rerank        carried chunks re-read from
 cannot state     → generate → validate    the DB + top-up retrieval,
 Ethiopian law                             then the same validation
        │               │                        │
        └───────────────┴────────────────────────┘
                        ▼
              internal/dialogue  ← implements qa.Answerer, dispatches
                        ▼
                 internal/qa (SSE, unchanged shape + two new fields)
```

`internal/qa` and its wire contract stay where they are; `dialogue` slots in
behind the existing `qa.Answerer` interface, so the handler does not change.

## The load-bearing decisions

**1. One model call classifies _and_ rewrites.** A follow-up already pays for a
condensation call. Adding a separate routing call would put two serial model
calls in front of retrieval and break the 3s TTFT requirement (SRS §14.1). The
router returns both in one structured response, so a follow-up costs the same as
today and a first question costs one small call more.
`internal/rag/followup.go`'s condensation moves into the router; its fallback
logic (`joinForRetrieval`, `cleanRewrite`) is reused.

**2. Safety comes from what the general engine _cannot do_, not from the
classifier being right.** The chat engine is handed no sources, a prompt that
forbids asserting Ethiopian law, and a post-generation guard. If the guard sees a
legal assertion — an article or proclamation reference, deontic phrasing about
what the law requires — the reply is **discarded** and the message falls through
to the legal engine. A misroute then costs latency, never correctness. This is
what makes it safe to let a model make the routing call at all.

**3. Failure routes to law, never to chat.** Router timeout, malformed JSON,
provider error → treat as a legal question. The expensive path is the safe one.

**4. Speculative retrieval hides the router's latency on first turns.** With no
history the rewrite is almost always the question itself, so retrieval starts in
parallel with the router and is discarded if the router says otherwise. On
follow-ups we wait, because the rewrite is the whole point. Costs one wasted
embedding on non-legal first turns — worth measuring against the free-tier quota.

**5. Carried sources are re-read from the database, never trusted from the
client.** The client sends `context_chunks` (chunk ids it is displaying). The
server resolves them through `citations.Resolver` and uses the stored text. A
client cannot inject text into a grounded answer.

**6. Multi-hop is gated.** A second retrieval pass driven by what the first
answer was missing is the largest quality lever and the largest latency risk. It
ships behind `ANSWER_MULTI_HOP` (default off) and turns on when the eval shows it
pays.

## Phase 0 — Evaluation harness first

Without this, every later phase is an opinion. `tests/evaluation/` is empty.

- `tests/evaluation/dataset/routing_v1.yaml` — ~60 labelled messages: greetings
  (English + Amharic), thanks, capability questions, plain legal questions,
  greeting-plus-question, generic-concept questions, follow-ups that need the
  rewrite, prompt-injection attempts.
- `tests/evaluation/routing_test.go` (`//go:build eval`) — router accuracy, and
  the asymmetric metric that matters: **legal-questions-routed-to-chat must be
  zero**, measured before and after the chat engine's guard.
- `tests/evaluation/answer_test.go` — the 50-question set from
  `docs/QA_FEATURE_PLAN.md` §8: Precision@5, citation accuracy, groundedness,
  abstention correctness, TTFT. Reused as the gate for Phases 2–4.
- Results committed under `tests/evaluation/results/` so later changes are
  compared, not argued.

## Phase 1 — The router and the general engine

**`internal/ai`** — the boundary needs structured output.

- `llm.go`: add `Complete(ctx, Request) (string, *Usage, error)` and
  `Request.ResponseSchema` / `ResponseMIMEType`. Gemini is the only implementer
  (`grep` confirms), so the interface change is cheap.
- `gemini.go`: `generateContent` (non-streaming) and `responseMimeType` +
  `responseSchema` in `generationConfig`, which the struct does not carry yet.

**`internal/router`** — new package, one job.

- `Decision{Intent, StandaloneQuestion, Depth, UsePriorSources, Language}`.
- Intents: `legal`, `discussion` (about sources already in the thread),
  `general` (concept/greeting/capability), `unsupported`.
- Strict JSON schema, ~120 output tokens, 4s timeout, fail-open to `legal`.
- Reuses `cleanRewrite` and `joinForRetrieval` from `internal/rag/followup.go`
  (moved here).

**`internal/chat`** — new package, the general engine.

- Prompt: may explain general concepts; must label them as general knowledge;
  must never state Ethiopian law; must offer to search the corpus. Seeded with
  the capability description currently in `smalltalk.go` (that copy is good —
  it is the mechanism around it that is wrong).
- `guard.go`: post-generation check for legal assertions → discard and signal
  fall-through.

**`internal/dialogue`** — new package, dispatch only. Implements `qa.Answerer`,
composes router + chat + rag, owns the fall-through, emits the `route` event.

**Deleted:** `internal/rag/smalltalk.go` and `smalltalk_test.go`. The service-level
tests in that file move to `internal/dialogue`.

**Wiring:** `cmd/api/main.go` builds router + chat + dialogue and passes
`dialogue.Service` where `rag.Service` goes today.

## Phase 2 — Conversation memory of citations

- `internal/qa/dto.go`: `context_chunks []string`, bounded (≤ 24, validated as
  the existing fields are).
- `internal/citations`: add a lenient `ResolveSome` — a stale chunk id must skip,
  not fail the answer. `Resolve`'s strict behaviour stays for the answer path,
  where a missing id _is_ a grounding failure.
- `internal/rag`: `Question.CarriedChunks`; resolved chunks convert to
  `search.Result` (`Retriever: "carried"`), merge with fresh retrieval, dedupe by
  chunk id, rerank the union, budget as now.
- `internal/rag/context.go`: mark carried sources in the block ("already shown to
  the reader in this conversation") so the model can refer back naturally.
- Frontend: `useQaConversation` sends the chunk ids from the thread's stored
  citations — `StoredTurn.citations[].chunkId` already has them, no new storage.

## Phase 3 — Wider, better-reasoned answers

- Router emits `depth`: `quote` (what does X say), `explain` (what does it mean),
  `analyse` (how do these apply together).
- `rag.Config.TopN` scales with depth — 6 for `quote`, up to 10 for `analyse`,
  still inside `ContextTokenBudget`.
- `grounding.go` prompt: per-depth guidance, building on the explain/reason rules
  added last week.
- `done` event gains `related`: follow-up suggestions derived **deterministically**
  from the citations' document and article metadata — no extra model call, and
  nothing invented.
- Frontend: suggested follow-ups as chips under an answer; "Discuss this source"
  on a citation card, which starts a follow-up carrying that chunk id.

## Phase 4 — Multi-hop retrieval (flagged off)

After the first retrieval, one structured call asks what is still missing —
article numbers, or a further query. Reuses the existing exact-article arm
(`articleLookup` in `internal/search/service.go`, promoted to a public
`ByArticles`). Second retrieval, merge, then a single generation.

`ANSWER_MULTI_HOP=false` by default. Turned on only if the eval shows citation
accuracy and groundedness improve enough to justify the latency.

## Wire contract changes

```
POST /api/v1/qa/query
  + "context_chunks": ["<chunk_id>", …]     chunks the client is displaying

event: route     data: {"intent":"legal","search_query":"…","carried_sources":3}
                 ↑ new, emitted before retrieval: lets the UI say what it is
                   actually searching for, and makes the rewrite visible
event: done      + "intent", "depth", "related":[{"question":"…"}]
```

`kind` (`legal` | `abstention` | `conversation`) already exists and is unchanged.
Frontend: `src/lib/api/{types,schemas,qa}.ts`, `src/features/qa/{useQaConversation,TurnView,SourcesPanel}.tsx`.

## Budget and risk

|                      | Today                 | After                                                  |
| -------------------- | --------------------- | ------------------------------------------------------ |
| First legal question | 1 call                | 1 router + 1 answer (retrieval speculated in parallel) |
| Follow-up            | 1 condense + 1 answer | unchanged — same two calls                             |
| Greeting             | 0 calls (phrase list) | 1 router + 1 short chat call                           |
| Multi-hop question   | —                     | +1 call, +1 retrieval, flagged off                     |

- **TTFT** is the constraint (< 3s, SRS §14.1). Measured per phase; speculative
  retrieval and a 4s router cap are the levers if it slips.
- **Free-tier quota** — every turn now costs a router call. Small (~120 output
  tokens), but real on the current key.
- **Misroute** — mitigated structurally by the guard and fall-through, measured
  by the routing eval.
- **Prompt injection through history** — roles are already validated in
  `qa/dto.go`; the router's output is constrained by schema; the marker validator
  is unaffected. The routing dataset includes injection attempts.

## Verification

Per phase, and all of it before saying it works:

1. `go build ./... && go vet ./... && go test ./...` in `seera-backend-services`;
   `npm run typecheck && npm run lint && npm run test && npm run build` in
   `seera-web`.
2. `go test -tags eval ./tests/evaluation/...` — routing accuracy, zero
   legal-to-chat misroutes, then Precision@5 / citation accuracy / groundedness /
   abstention against the committed baseline.
3. Against the running stack (`docker compose --profile full up -d --build`):
   - "Hello" → conversational reply, no sources panel, no grounding badge.
   - "What is a private limited company?" → general concept, labelled as general
     knowledge, offers to search.
   - "How do I register a company?" → grounded answer with citations.
   - Then "what about for a PLC?" → the `route` event shows the rewritten query;
     the answer reuses the carried sources.
   - Then "explain the second one in plain language" → discussion path, same
     citations, deeper explanation.
   - A question with no coverage → abstention, unchanged.
4. `curl -N` the endpoint directly to confirm event order and the new fields.
5. TTFT recorded from the `done` event's `timings_ms` for each case above and
   compared against the < 3s target.
