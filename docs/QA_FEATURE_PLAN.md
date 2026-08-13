# Feature 1 — AI Legal Q&A: Implementation Plan

**Status:** planned, not started
**Created:** 2026-08-14
**Scope rule:** this is the only feature under construction. Knowledge Explorer, Business
Advisor, and Document Intelligence are **out of scope** until Q&A meets its exit gates —
even though `internal/business/`, `internal/documents/`, and `internal/legal/` handlers
exist as empty placeholders.

---

## 1. The milestone

> Given 10–20 Ethiopian legal documents, answer 50 predefined legal questions with
> accurate article-level citations, streamed to a React UI, with measured accuracy.

Success is a number, not an impression. The feature is done when the gates in §9 pass.

### Vertical slice

```
Legal PDFs (EN + AM)
  → offline Python extraction (PyMuPDF) → structured JSON
  → Go ingestion: article chunking + metadata + embeddings
  → PostgreSQL 15+ / pgvector + FTS
  → hybrid retrieval (vector ∪ BM25 ∪ exact article lookup) → RRF
  → Cohere rerank → top 6
  → Claude generation with citation-required prompt
  → Go-side citation validation (grounding gate)
  → SSE stream
  → React chat + citation cards + source viewer
```

---

## 2. Decisions locked

| Area | Decision | Notes |
|---|---|---|
| Embeddings | OpenAI `text-embedding-3-large`, `dimensions=1536` | 1536 keeps it under pgvector's 2000-dim HNSW ceiling for `vector`. Behind `ai.Embedder`. |
| Ingestion | Offline Python script → JSON → Go ingest | No worker service. Parse quality is inspected per document. |
| Reranker | Cohere Rerank (multilingual) | Behind `rag.Reranker`. One HTTP call, no infra. |
| Generation | Anthropic, `claude-opus-5` via official `anthropic-sdk-go` | Behind `ai.LLM`. SDK types never leave `internal/ai/anthropic.go`. |
| Corpus | **English + Amharic from day one** | 10–20 docs; see the risk in §2.1. |
| HTTP | `chi` router, REST + SSE | Per SRS §7.2. |
| DB access | `pgx` v5, hand-written SQL | No ORM. |
| Frontend | Vite SPA (React 19 + TS) | **Not** Next.js — the PRD/SRS say Next.js, the `project-architecture` skill and the actual repo say Vite. Vite wins; the docs are stale. |

### 2.1 The one tension worth naming

Amharic on day one + OpenAI embeddings is a known-weak pairing. `text-embedding-3-large`
is not evaluated for Amharic and will likely underperform there. Mitigations, all in the plan:

1. Hybrid search carries Amharic harder than English — lexical + exact article lookup do
   not depend on the embedding model at all.
2. The schema stores `embedding_model` + `embedding_version` per row, and each embedding
   space gets **its own table** (dims differ per model), so a second model can be indexed
   alongside for A/B without a destructive migration.
3. Phase 4 reports **Precision@5 split by language**. Gate: if Amharic P@5 < 0.65 while
   English passes, swap the embedder to Jina v3 or self-hosted BGE-M3 and re-embed. That
   is a wiring change plus a backfill, not a rewrite.

Budget for the possibility that Amharic forces the embedder swap. Do not treat the OpenAI
choice as final.

---

## 3. Package layout and dependency direction

Import direction is one-way. Violating it means a cycle, and the compiler will say so late.

```
cmd/api
   └→ internal/server ─→ internal/qa ─→ internal/rag ─→ internal/search ─→ internal/legal
                                            │              │                    │
                                            ├→ internal/citations ──────────────┤
                                            └→ internal/ai                      │
                     internal/ingestion ─→ internal/ai, internal/legal ─────────┤
                                                                    internal/database
```

- `internal/legal` is the **domain package**: `Document`, `Version`, `Article`, `Chunk`,
  `Language`, `DocType`. It imports nothing from siblings. The old tree's `internal/domain`
  role now lives here.
- `internal/citations` owns citation resolution + the `Citation` payload type.
- `internal/ai` owns `LLM` and `Embedder` interfaces plus the OpenAI/Anthropic adapters.
  `local.go` stays empty for now.
- `internal/rag` orchestrates; it never talks to pgx directly — it goes through
  `internal/search` and `internal/citations`.
- `internal/business`, `internal/documents`, `internal/qa/…` beyond what's listed: untouched.

**Phase 0 chore:** `.claude/skills/project-architecture/SKILL.md` still documents the old
layout (`internal/api`, `internal/service`, `internal/repository`, `internal/domain`,
`internal/provider`, `migrations/`, `workers/`). It is a rules file Claude follows — update
its "Where things go" table to match the tree above before writing code, or it will keep
steering work into deleted directories.

**One deviation from the agreed folder tree:** the Python extraction script needs a home.
Add `workers/extraction/` back (script + `requirements.txt` + README). It is not a service
and is not imported by Go; it is run by hand and produces JSON.

---

## 4. Phase 0 — Foundation

Nothing in Phases 1–5 can run without this. Target: a server that boots, connects, and
answers `/healthz`.

- `internal/config/config.go` — env-driven config: `DATABASE_URL`, `OPENAI_API_KEY`,
  `ANTHROPIC_API_KEY`, `COHERE_API_KEY`, `PORT`, `LOG_LEVEL`, `ENV`. Fail fast on missing
  required keys; never log values.
- `internal/database/postgres.go` — pgx pool, ping on boot, `Close()` on shutdown.
- `internal/database/migrations/` — plain numbered SQL (`0001_extensions.up.sql`, …),
  applied with `golang-migrate` or a 40-line embedded runner. Pick one and don't revisit.
- `internal/server/server.go` — chi router, timeouts, graceful shutdown.
- `internal/server/middleware.go` — request ID, structured logging (`log/slog`), recovery,
  CORS for the Vite dev origin, body-size limit, per-IP rate limit on `/qa`.
- `internal/server/routes.go` — route table, one place.
- `cmd/api/main.go` — wiring only: config → db → providers → services → handlers → server.
- Go deps to add: `chi`, `anthropic-sdk-go`. OpenAI and Cohere are called with stdlib
  `net/http` (two endpoints each; a full SDK is not worth the dependency).
- `docker-compose.yml` — `pgvector/pgvector:pg16` with a healthcheck.
- `Dockerfile` — multi-stage, distroless final.

**Exit:** `docker compose up` + `go run ./cmd/api` → `GET /healthz` returns `{"status":"ok","db":"up"}`.

---

## 5. Phase 1 — Knowledge

### 5.1 Corpus

10–20 documents. Bias toward commercial/business law so the 50 questions have real answers
in-corpus, and include at least 3 Amharic documents so the language split in §9 is
meaningful. For each: title, doc type, issuing authority, publication + effective date,
language, source URL, and the PDF itself.

### 5.2 Extraction (Python, offline)

`workers/extraction/extract.py` — PyMuPDF, one PDF in, one JSON out:

```json
{
  "source_file": "commercial_code_2021.pdf",
  "language": "en",
  "page_count": 412,
  "articles": [
    {
      "article_no": "45",
      "title": "Formation of private limited companies",
      "chapter": "Companies",
      "section_path": ["Book II", "Title V", "Chapter 3"],
      "text": "...",
      "page_no": 88,
      "char_start": 145203,
      "char_end": 147891,
      "structure_confidence": "high"
    }
  ]
}
```

Rules:
- Detect structure by regex over the extracted text: `Article\s+(\d+)`, `አንቀጽ\s+([፩-፼\d]+)`,
  chapter/part/section headers. **Ge'ez numerals must be normalized to Arabic** — build the
  mapping once, in the script, and emit both the raw and normalized form.
- If article detection fails for a document, fall back to ~512-token windows with 15%
  overlap and mark every chunk `structure_confidence: "low"`. Do not silently ship it —
  the script prints a per-document report (articles found, pages with no text, OCR needed).
- OCR (`pytesseract` + `amh` traineddata) only for pages where the text layer is empty.
- Output goes to `workers/extraction/out/*.json`, gitignored. **Eyeball every file before
  ingesting it.** This is where retrieval accuracy is won or lost, and it is cheap to check
  at 20 documents and impossible at 2000.

### 5.3 Schema

```sql
-- 0001_extensions
CREATE EXTENSION vector;  CREATE EXTENSION pg_trgm;  CREATE EXTENSION unaccent;

-- 0002_legal
legal_documents(id, title, doc_type, legal_domain, issuing_authority,
                publication_date, effective_date, language, source_url,
                storage_key, created_at)

document_versions(id, document_id FK, version_label, effective_from, repealed_at,
                  status /* draft|published|superseded */, published_at,
                  UNIQUE(document_id, version_label))

articles(id, version_id FK, document_id FK, article_no, article_no_raw, title,
         chapter, section_path text[], text, ordinal, page_no,
         char_start, char_end, structure_confidence,
         UNIQUE(version_id, ordinal))

chunks(id, article_id FK, version_id FK, document_id FK, language,
       text,              -- raw span, what the UI shows
       embed_text,        -- contextualized: title + chapter + article no + text
       token_count, ordinal,
       fts tsvector GENERATED ALWAYS AS (to_tsvector(fts_config, embed_text)) STORED,
       fts_config regconfig)   -- 'english' for en, 'simple' for am

-- 0003_embeddings (one table per embedding space; dims are per-model)
chunk_embeddings_openai_1536(chunk_id PK FK, embedding vector(1536),
                             model text, model_version text, created_at)
```

Indexes: HNSW `vector_cosine_ops` on the embedding; GIN on `chunks.fts`; GIN
`gin_trgm_ops` on `chunks.text` (Amharic fuzzy — Postgres has no Amharic dictionary, so
`simple` + trigram is the honest configuration); btree on `(document_id, version_id)`,
`(version_id, article_no)`, and `document_versions.status`.

Versioning rule (SRS §19, architecture rule 10): amendments create a new
`document_versions` row. Nothing is ever updated in place. Publishing is one transaction —
version + articles + chunks + embeddings inserted, validated, *then* `status='published'`.
A failed ingest leaves nothing queryable.

### 5.4 Ingestion (Go)

`internal/ingestion/`: `parser.go` reads the JSON contract, `chunker.go` builds chunks from
articles, `embedder.go` batches (≤96 inputs, retries with backoff), `service.go` runs the
transaction.

Chunking: **one chunk per article** by default. Split only when an article exceeds ~1000
tokens, at sub-article/paragraph boundaries, and keep the parent article link. `embed_text`
always carries the contextual header — an isolated *"(2) The period shall be thirty days."*
is unretrievable without it.

Trigger: `POST /api/v1/admin/documents/ingest` with the JSON body + metadata, guarded by a
static admin token from config. Keeps the single-binary shape; no `cmd/ingest` needed.

**Exit:** all documents published; `SELECT count(*) FROM chunks` is a number you can
justify; spot-checking 10 random chunks shows complete, correctly-attributed provisions.

---

## 6. Phase 2 — Retrieval

`internal/search/` — three retrievers, one fuser.

| File | Job |
|---|---|
| `vector.go` | Embed query → HNSW kNN, top 50, `published` versions only |
| `keyword.go` | `websearch_to_tsquery` over `chunks.fts`, top 50; trigram fallback for Amharic |
| `hybrid.go` | Reciprocal Rank Fusion, `k=60`. No score-weight tuning. |
| `service.go` | Filters, the exact-article branch, orchestration |

**Exact-article branch** (the highest-leverage legal-specific win): regex the query for
`Article 45`, `Art. 45(2)`, `Proclamation No. 1051/2017`, `አንቀጽ ፵፭`. On a hit, do a direct
SQL lookup and inject those chunks into the candidate pool at rank 1. Legal users search by
citation constantly, and neither embeddings nor BM25 reliably nail it.

Filters are **SQL predicates**, never prompt instructions: `status='published'`, and
in-force — `effective_from <= $date AND (repealed_at IS NULL OR repealed_at > $date)`.

`internal/rag/reranker.go` — Cohere, 50 candidates → top 6, behind an interface with a
pass-through no-op implementation so the eval harness can measure with and without.

Every retrieved chunk carries `chunk_id, document_id, document_title, article_no,
section_path, version_id, effective_from, language, score` from here to the response. The
metadata is not optional and is never dropped mid-pipeline.

**Exit:** retrieval-only eval (no LLM) on the 50 questions reports Precision@5 and Recall@10,
overall and per language.

---

## 7. Phase 3 — Generation

`internal/rag/context.go` — build the sources block: numbered chunks, each with a metadata
header and its `chunk_id`. Hard token budget; drop from the tail, never the head.

`internal/rag/service.go` — orchestrate: normalize → retrieve → filter → rerank → abstain
check → build context → stream → validate.

`internal/ai/llm.go`:

```go
type LLM interface {
    Stream(ctx context.Context, req Request) (Stream, error)
}
```

`internal/ai/anthropic.go` — `claude-opus-5` via `anthropic-sdk-go`, streaming. Two model
settings to establish empirically in Phase 4, not now: `output_config.effort` (start `low`
or `medium` — this path is retrieval-bound, not reasoning-bound, and TTFT must stay under
3s) and whether adaptive thinking stays on. Note that on `claude-opus-5` thinking is **on by
default**, and `max_tokens` caps thinking + text together — size it accordingly. If TTFT
misses the target at every effort level, `claude-sonnet-5` is the lever to try before
touching the prompt.

**Prompt contract** (`grounding.go` owns the system prompt):
- Answer **only** from the provided sources.
- Cite every legal claim as `[chunk_id]`.
- If the sources are insufficient, say so explicitly — do not answer from general knowledge.
- Distinguish what the law says from how it is applied. No individual legal advice.
- **No legal text in the system prompt.** It comes from retrieval or it does not exist.

**Post-generation validation, in Go, not optional:**
1. Parse `[chunk_id]` markers from the answer.
2. Every marker must resolve to a chunk in *this request's* retrieved set. Unknown marker →
   strip the claim and log it.
3. Non-empty legal content with zero citations → replace with the abstain response. That
   state is a bug, and it should look like one in the logs.
4. Attach resolved `Citation` objects to the payload. The frontend renders from those, never
   by parsing answer text.

**Abstention** (`grounding.go`): abstain when the fused top score or the reranker top score
is below threshold, or every candidate fails the in-force filter. The abstain response names
what was searched and suggests a narrowing. It does not apologize and then guess.

### API contract

```
POST /api/v1/qa/query        → text/event-stream
GET  /api/v1/citations/{id}  → JSON, the resolved chunk + article + version metadata
```

Request: `{"question": "...", "language": "en"|"am"|null, "as_of": "2026-08-14"|null}`

SSE events, in order:

```
event: sources     data: {"citations":[{...}]}     # after retrieval, before the first token
event: token       data: {"text":"A private "}
event: done        data: {"citations":[...],"abstained":false,"latency_ms":{...},"request_id":"..."}
event: error       data: {"code":"...","message":"..."}
```

Emitting `sources` before generation lets the UI render citation cards while the answer
streams, which is most of the perceived-latency win.

`GET /api/v1/citations/{id}` is the citation resolver only. It is **not** a document browse
or search endpoint — that is Feature 2, and building it now is scope creep.

Observability per SRS §22: request ID, retrieval latency, chunk count, rerank latency, LLM
latency, TTFT, token usage, citation IDs. Never log document text or full prompts.

**Exit:** `curl -N` against `/qa/query` streams a grounded, cited answer; an
out-of-corpus question abstains.

---

## 8. Phase 4 — Evaluation

This phase is why the feature is credible. It runs before the UI so that UI work is spent
on something known to be correct.

`tests/evaluation/dataset/qa_v1.yaml` — 50 questions, hand-built:

```yaml
- id: qa-001
  question: "What are the requirements for forming a private limited company?"
  language: en
  expected_documents: ["commercial-code-2021"]
  expected_articles: ["45", "46"]
  expected_supporting_text: "A private limited company may be formed by..."
  should_abstain: false
```

Mix deliberately: ~30 answerable English, ~12 answerable Amharic, ~5 citation-style
("What does Article 45 say?"), ~3 out-of-corpus (`should_abstain: true`).

Harness in `tests/evaluation/`, Go, behind a `//go:build eval` tag so `go test ./...` stays
fast and offline. Two independently runnable stages — a bad answer must be attributable to
retrieval or to generation, never ambiguously both:

| Metric | Stage | Target |
|---|---|---|
| Precision@5 | retrieval only | ≥ 0.80 overall, ≥ 0.65 Amharic |
| Recall@10 | retrieval only | ≥ 0.90 |
| Citation accuracy | generation | ≥ 0.90 |
| Groundedness (no uncited legal claims) | generation | ≥ 0.90 |
| Abstention correctness | generation | 100% on the out-of-corpus set |
| TTFT / total latency | end-to-end | < 3s / < 10s |

Output a run report (JSON + a printed table) with per-question pass/fail, committed under
`tests/evaluation/results/` so changes to chunking, fusion, or reranking are measured
against a baseline rather than argued about.

**Exit gate — do not start Phase 5 until every target above passes.** If Amharic P@5
fails while English passes, execute the embedder swap in §2.1 and re-run before proceeding.

---

## 9. Phase 5 — Product UI

`seera-web`, Vite SPA. Setup first: Tailwind, `VITE_API_BASE_URL`, dev proxy to `:8080`.
No router needed — one page.

```
src/
├── features/qa/
│   ├── ChatPanel.tsx        # layout, disclaimer, suggested questions
│   ├── MessageList.tsx
│   ├── AnswerStream.tsx     # renders tokens + inline citation markers
│   ├── CitationCard.tsx     # doc title, article no, version, effective date, snippet
│   ├── SourceDrawer.tsx     # full article text, metadata, source URL
│   ├── useQaStream.ts       # POST + ReadableStream SSE parser
│   └── types.ts             # mirrors the Go DTOs
└── lib/api.ts
```

Two things that will otherwise bite:

- **`EventSource` cannot POST.** Use `fetch` + `response.body.getReader()` +
  `TextDecoder`, and hand-parse the `event:`/`data:` frames. Buffer partial frames across
  chunk boundaries — an SSE frame does not respect network chunking.
- **Citation markers render from the `citations` payload**, matched by `chunk_id`. Never
  regex the answer text in the client.

Required states: idle with suggested questions, retrieving, streaming, abstained, error,
network-dropped. Plus the legal disclaimer visible on the page (PRD §14), Amharic text
rendered with a font that has proper Ethiopic coverage, and keyboard-navigable citation
cards.

**Exit:** a user types a question and sees a streaming, cited answer; clicking any citation
shows the exact source text and its version metadata.

---

## 10. Risks

| Risk | Mitigation |
|---|---|
| Amharic retrieval underperforms | Split-language eval gate; embedder swap path pre-built (§2.1) |
| PDF structure detection fails on some docs | Per-document parse report; `structure_confidence: low` marks degraded chunks; window fallback |
| TTFT misses 3s | `sources` event pre-generation; effort tuning; `claude-sonnet-5` as the fallback lever |
| 50 questions have no in-corpus answer | Build the dataset **against the ingested corpus**, not before it |
| Reranker adds a hard vendor dependency | `Reranker` interface with a no-op impl; eval measures the delta so its value is known, not assumed |
| Scope creep into Explorer / Advisor | Empty package dirs are placeholders. No handler, route, or migration for them lands before Q&A's gates pass. |

## 11. Open items

1. Where do the source PDFs live? S3-compatible storage is specified (`internal/storage/`),
   but for 20 admin-uploaded documents local disk + a `storage_key` column is enough for
   Phase 1. **Recommendation:** implement `storage.Storage` with a filesystem backend now,
   `s3.go` when there is a deployment target.
2. Effective-date filtering needs real `effective_from` / `repealed_at` values per document.
   If they can't be sourced reliably, the filter must default to permissive and the answer
   must state the version it relied on.
3. Admin auth for the ingest endpoint — a static bearer token from config is proposed. Real
   auth is explicitly out of MVP scope (PRD §15).
