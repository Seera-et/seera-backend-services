# seera-backend-services

Go API and offline workers for Seera — a grounded question-answering system over
Ethiopian legal documents.

```
React 19 + TS + Vite (seera-web)
        │  HTTP / JSON + SSE
        ▼
Go REST API  ──►  retrieval → rerank → generation → grounding
        │
        ▼
PostgreSQL + pgvector          Gemini (embeddings + generation)
```

Python appears only under [workers/](workers/), offline, for PDF extraction. The
architecture rules are in
[.claude/skills/project-architecture/](.claude/skills/project-architecture/SKILL.md)
and apply to every change.

## Requirements

- Go 1.26+
- Docker (Postgres 16 + pgvector)
- A `GEMINI_API_KEY` — one key covers both embeddings and generation
- Python 3.11+ only if you are extracting PDFs

## Local setup

```bash
cp .env.example .env          # then add GEMINI_API_KEY and ADMIN_API_TOKEN
docker compose up -d          # Postgres + pgvector on :5432

set -a && source .env && set +a   # the Go app does not read .env itself
go run ./cmd/api                  # migrations run on boot
```

```bash
curl localhost:8081/healthz   # {"status":"ok","db":"up"} — checks the database
```

Config is read from the environment once at startup and validated, so a missing
`DATABASE_URL` fails immediately rather than at first query. A feature whose
credential is absent leaves its route **unregistered** rather than failing on
every call — so a 404 on `/api/v1/qa/query` means a missing key, and the startup
log says which.

To run the API in a container instead (this verifies the image that ships):

```bash
docker compose --profile full up --build
```

## Ingesting a document

Extraction is offline and by hand; parse quality is won by reading the output.

```bash
cd workers/extraction
python -m venv ../.venv && ../.venv/Scripts/pip install -r requirements.txt

python triage.py "path/to/pdfs" --json triage.json   # what is usable at all
python extract.py "path/to/code.pdf" --language en --out out/
```

Then POST the JSON with its metadata — see
[workers/extraction/README.md](workers/extraction/README.md) for the full request
shape. `publish` defaults to `false`: a document is invisible to retrieval until
someone has read its parse report.

**Expect ingestion to be slow on a free Gemini key.** The embedding quota is 100
texts per minute, so an 800-article code takes about ten minutes. The API honours
the delay the provider states and logs each wait, so a slow run is
distinguishable from a stuck one.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness plus a real database check |
| `POST /api/v1/qa/query` | Ask anything; routed, then streamed as Server-Sent Events |
| `GET /api/v1/citations/{id}` | Resolve a citation to its exact source text |
| `GET /api/v1/corpus/stats` | What the corpus contains, over published versions |
| `GET /api/v1/documents` | Browse the catalogue (`q`, `language`, `type`, `limit`, `offset`) |
| `GET /api/v1/documents/{id}` | One document and every version a reader may see |
| `GET /api/v1/documents/{id}/articles` | A version's articles, keyset-paginated (`version`, `after`, `limit`) |
| `POST /api/v1/admin/documents/ingest` | Ingest an extraction (bearer token) |

The answer stream is ordered `route` → `sources` → `token`* → `done`, with
`error` in place of `done` on failure. `route` says which engine is answering and
what retrieval was actually given; `sources` arrives before the first token so the
UI can render citation cards while the answer streams.

The catalogue endpoints read **published versions only** — the same set retrieval
sees, so a document that can be browsed is a document that can be cited, and the
count on the home page is a claim the system can stand behind. Superseded
versions are readable through `GET /documents/{id}`; drafts never are.

### Conversations

`POST /api/v1/qa/query` accepts an optional `history` of earlier turns:

```json
{
  "question": "What about for a PLC?",
  "history": [
    {"role": "user",      "text": "How do I register a company in Ethiopia?"},
    {"role": "assistant", "text": "..."}
  ]
}
```

Nothing is stored server-side. The client owns the conversation and sends what it
wants considered, which keeps the "no answer text is persisted" property intact
and sidesteps owning a conversation with no authentication in place.

The rewrite that makes a follow-up searchable is done by the router (below),
which returns it in the same call that classifies the message. Its cost shows up
as `condense` in the `timings_ms` of the `done` event.

Earlier turns also reach the generator, labelled as context and not as a source.
They cannot become grounding: marker validation still runs against the chunks
retrieved for this request.

### Routing

Every message goes through a router before anything is retrieved
(`internal/router`). One structured model call returns the intent, the depth, and
the question rewritten to stand on its own — classification and the follow-up
rewrite together, because two serial calls in front of retrieval would spend the
time-to-first-token budget on plumbing.

```
                 internal/router          classify + rewrite, one call
                        │
        ┌───────────────┼────────────────────────┐
        ▼               ▼                        ▼
 internal/chat    internal/rag             internal/rag
 general reply    legal answer             discussion: carried sources
        │               │                        │
        └───────────────┴────────────────────────┘
                 internal/dialogue  ← dispatch, fall-through, route event
```

Three intents:

| Intent | Engine | |
|---|---|---|
| `legal` | `internal/rag` | Retrieve, rerank, generate, validate every citation |
| `discussion` | `internal/rag` | The same, carrying the provisions already cited in the thread |
| `general` | `internal/chat` | Greetings, what Seera does, and general concepts — no corpus, no citations |

**Safety does not rest on the classifier being right.** The general engine is
given no sources and a prompt that forbids asserting Ethiopian law, and every
reply it produces is checked before anyone sees it (`internal/chat/guard.go`). A
reply that names an article, an instrument, or an obligation under Ethiopian law
is discarded and the message is answered from the corpus instead. A misroute
therefore costs latency, not correctness.

Failures route to the law: a router timeout, a malformed decision or a provider
outage all resolve to `legal`, which is the expensive path and the safe one.

On the first turn of a conversation, retrieval starts in parallel with the router
and is used only if the router agrees the message is legal and did not change the
query. That hides the router's latency on the turn where the question is already
the search query.

### Conversation memory

`context_chunks` carries the chunk ids of the citations the client is displaying.
The server re-reads them from the database (`citations.ResolveSome`, lenient about
ids that no longer exist) and puts them at the front of the context, so "explain
the second one" is answered from the provision already on screen rather than from
whatever retrieval finds for a pronoun. Identifiers only — text from a client can
direct an answer, never supply its evidence.

### When an answer is less than it looks

Two failures used to reach the reader wearing a "Grounded" badge.

**The provisions did not settle it.** "The sources do not contain the
requirements for a business licence" is a well-cited, perfectly grounded
sentence, and nothing in the citation machinery can tell it apart from an
answer. So the generation contract asks for a sentinel — the reply begins with
`NOT_IN_SOURCES` — which the server strips before any of it streams and reports
as `kind: "insufficient"`. Sources are still returned: they were read, and they
are worth reading.

**The answer was cut off.** The provider's `finishReason` was parsed and
discarded, so an answer that hit the output limit and stopped mid-sentence
looked finished. It now sets `truncated` on the `done` event. The output budget
also rose from 2048 to 4096 tokens, because the prompt now asks for the
provision *and* an explanation of it, and Amharic costs about a token per
character.

### Answer depth and the second pass

The router also picks a depth: `quote` (fewer provisions, the text itself),
`explain` (the default), `analyse` (more provisions, applied together). It scales
how many reach the generator, inside the same context budget.

`ANSWER_MULTI_HOP=true` enables a second retrieval pass: after the first results
come back, the model is asked which further articles they cross-reference, and
those are fetched too. Off by default — it costs a model call and a retrieval on
every answer that uses it, and that is charged to time-to-first-token. Turn it on
when the evaluation says the trade is worth it.

### Evaluation

```bash
set -a && source .env && set +a
go test -tags eval ./tests/evaluation/... -v
```

`tests/evaluation/dataset/routing_v1.yaml` holds ~50 labelled messages: greetings
in both languages, capability questions, plain legal questions, greetings with a
question attached, follow-ups that need the rewrite, questions about sources
already on screen, and attempts to talk the router out of its job.

Two numbers come out, and they are not equally important. Exact intent accuracy
is informational — confusing `legal` with `discussion` costs nothing, since both
retrieve. **Questions needing the corpus that were routed to the general engine
must be zero**, and that assertion fails the build. The dataset's shape is checked
by the ordinary test run, so a malformed one does not wait for a tagged run to be
noticed.

## Layout

```
cmd/api/                    HTTP entrypoint — wiring only
internal/
  server/                   router, middleware, SSE plumbing, lifecycle
  qa/                       the Q&A endpoint and its SSE contract
  rag/                      context building, grounding, reranking, orchestration
  search/                   hybrid retrieval: vector, keyword, exact article
  citations/                citation resolution
  ingestion/                chunking, embedding, indexing, publishing
  legal/                    domain types and their SQL
  ai/                       LLM and embedding providers behind interfaces
  database/                 pgx pool and forward-only migrations
  config/                   environment loading and validation
pkg/logger/                 structured logging
workers/extraction/         Python: PDF triage and extraction (offline)
tests/                      integration and RAG evaluation
docs/                       PRD, SRS, the Q&A plan, the corpus assessment
```

Dependencies point one way. `internal/legal` imports no sibling package;
`internal/rag` never touches pgx, going through `search` and `citations` instead.

## Working on this repo

Read the relevant skill in [.claude/skills/](.claude/skills/) before changing a
layer — they carry the rules that keep legal answers grounded and citable:

`project-architecture`, `legal-rag`, `backend-go`, `database`,
`document-ingestion`, `testing-evaluation`, `frontend-react`.

Four that are easy to violate by accident:

- Legal claims are grounded in retrieved chunks, and citations point at internal
  records (`chunk_id`, `version_id`) — never a model-authored citation.
- Document versions are never overwritten or deleted. Amendment and repeal add
  rows.
- No legal text in Go source or in prompts. It lives in the database or it does
  not exist.
- Jurisdiction and effective-date filtering are SQL predicates, not prompt
  instructions.

Current state and what is measured: [docs/QA_FEATURE_PLAN.md](docs/QA_FEATURE_PLAN.md).
What the corpus actually contains: [docs/CORPUS.md](docs/CORPUS.md).
