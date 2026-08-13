# seera-backend-services

Go API and workers for Seera — a grounded question-answering system over
Ethiopian legal documents.

```
React 19 + TS + Vite (seera-web)
        │  HTTP / JSON + SSE
        ▼
Go REST API  ──►  Services  ──►  PostgreSQL + pgvector
```

Python appears only under [workers/](workers/) for document and ML processing.
The architecture rules are in [.claude/skills/project-architecture/](.claude/skills/project-architecture/SKILL.md)
and apply to every change.

## Requirements

- Go 1.26+
- Docker (Postgres 17 + pgvector)

## Local setup

```bash
cp .env.example .env
make db-up          # Postgres + pgvector on :5432
set -a && source .env && set +a
make run            # API on :8080
```

```bash
curl localhost:8080/healthz   # process is up
curl localhost:8080/readyz    # process can reach Postgres
```

Config is read from the environment once at startup and validated; a missing
`DATABASE_URL` fails immediately rather than at first query.

## Commands

`make help` lists everything. The common ones:

| Command | Purpose |
|---|---|
| `make run` / `make worker` | Run the API / the background worker |
| `make test` | Unit tests with the race detector |
| `make test-integration` | Tests against a real Postgres+pgvector |
| `make check` | fmt, vet, lint and test — what CI runs |
| `make db-up` / `db-reset` / `db-psql` | Local database |

## Layout

```
cmd/api/          HTTP entrypoint — wiring only
cmd/worker/       background jobs
internal/
  api/            routing, middleware, DTOs, SSE framing
  service/        business logic: retrieval, answer, ingest, auth
  repository/     every SQL statement, via pgx
  domain/         entities and provider interfaces
  provider/       LLM, embedding and rerank adapters
  apperr/         error kinds → HTTP status
  platform/       config, logging, database pool
migrations/       versioned, forward-only SQL
workers/          Python: PDF/OCR/structure detection
eval/             RAG evaluation datasets and results
```

Dependencies point one way: `api → service → repository → db`. `domain` imports
nothing from the application, and nothing imports `api`.

## API

Implemented:

- `GET /healthz` — liveness, touches no dependency
- `GET /readyz` — readiness, requires a reachable database

Planned, with the SSE answer contract already framed in
[internal/api/sse.go](internal/api/sse.go):

- `POST /v1/ask` — streams `sources` → `token`* → `citation`* → `done`
- `GET /v1/documents`, `GET /v1/documents/{id}`, `GET /v1/chunks/{id}`

Every error response is `{"error":{"code","message","request_id"}}`. Clients
switch on `code`; internal causes are logged, never serialized.

## Working on this repo

Read the relevant skill in [.claude/skills/](.claude/skills/) before changing a
layer — they carry the rules that keep legal answers grounded and citable:

`project-architecture`, `legal-rag`, `backend-go`, `database`,
`document-ingestion`, `testing-evaluation`, `frontend-react`.

Two that are easy to violate by accident:

- Legal claims are grounded in retrieved chunks, and citations point at
  internal records (`chunk_id`, `version_id`). Never a model-authored citation.
- Document versions are never overwritten or deleted. Amendment and repeal add
  rows.
