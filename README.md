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
| `POST /api/v1/qa/query` | Ask a legal question; streams Server-Sent Events |
| `GET /api/v1/citations/{id}` | Resolve a citation to its exact source text |
| `GET /api/v1/corpus/stats` | What the corpus contains, over published versions |
| `GET /api/v1/documents` | Browse the catalogue (`q`, `language`, `type`, `limit`, `offset`) |
| `GET /api/v1/documents/{id}` | One document and every version a reader may see |
| `GET /api/v1/documents/{id}/articles` | A version's articles, keyset-paginated (`version`, `after`, `limit`) |
| `POST /api/v1/admin/documents/ingest` | Ingest an extraction (bearer token) |

The answer stream is ordered `sources` → `token`* → `done`, with `error` in place
of `done` on failure. `sources` arrives before the first token so the UI can
render citation cards while the answer streams.

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

When `history` is present the service rewrites the question into a standalone
form before retrieval — "what about for a PLC?" carries none of the terms that
would find the provision. The rewrite is bounded (one sentence, 5s, capped
tokens) and every failure path degrades to the previous question joined to the
current one rather than failing the answer. Its cost shows up as `condense` in
the `timings_ms` of the `done` event.

Earlier turns also reach the generator, labelled as context and not as a source.
They cannot become grounding: marker validation still runs against the chunks
retrieved for this request.

### Messages that are not legal questions

"Hello" is not a retrieval problem. An intent gate in front of retrieval
(`internal/rag/smalltalk.go`) recognises greetings, thanks, farewells and
questions about what Seera does, and answers them from written replies — no
retrieval, no model call, no latency.

The classifier is deliberately lopsided. Greetings match the whole message, so
"hi, what does Article 5 say?" is a legal question; capability phrases match only
inside short messages; everything unrecognised is legal. Sending a greeting to
retrieval produces an awkward "not found". Sending a legal question to a canned
reply produces a statement about Ethiopian law that no source supports, and that
is the one thing this system must not do.

The `done` event carries `kind`: `legal`, `abstention` or `conversation`. The
client presents the three differently, and a conversational reply arrives with no
citations and neither `grounded` nor `abstained` set, so no client reading only
the booleans can dress it as a grounded legal answer.

Every JSON error is `{"error":{"code","message","request_id"}}`. Clients switch on
`code`; internal causes are logged against the request ID, never serialised.

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
