---
name: backend-go
description: Use when writing or reviewing Go code in seera-backend-services — HTTP handlers, SSE streaming, services, repositories, pgx usage, validation, error handling, concurrency, wiring, or Go tests.
---

# Backend (Go)

Single Go binary. Standard library HTTP where possible; `chi` if routing needs
grow. `pgx/v5` for Postgres. No ORM, no DI framework, no codegen unless it earns
its place.

## Layout

Module `github.com/Seera-et/seera-backend-services`, repo root:

```
cmd/api/main.go              # wiring only
cmd/worker/main.go           # ingestion jobs, re-embedding
internal/
  api/           handlers, DTOs, middleware, SSE framing
  service/       retrieval, answer, ingest, auth — business logic
  repository/    pgx queries, one file per aggregate
  domain/        entities + interfaces (Embedder, LLM, Reranker)
  provider/      LLM/embedding/rerank adapters
  apperr/        error kinds and their HTTP mapping
  platform/      config, logging, db pool
migrations/
workers/                     # Python, see document-ingestion
```

Dependency direction: `api → service → repository → db`. `domain` depends on
nothing. Nothing imports `api`.

## Dependency injection

Constructor injection, wired by hand in `main.go`. Interfaces are declared by the
**consumer** package, kept small, and accept concrete structs from `main`.

```go
type Retriever interface {
    Search(ctx context.Context, q domain.Query) ([]domain.Chunk, error)
}

type AnswerService struct {
    retriever Retriever
    llm       domain.LLM
    log       *slog.Logger
}

func NewAnswerService(r Retriever, l domain.LLM, log *slog.Logger) *AnswerService {
    return &AnswerService{retriever: r, llm: l, log: log}
}
```

No global state. No `init()` side effects. No service locator.

## Handlers

Thin: decode → validate → call service → encode. No SQL, no business rules.

```go
func (h *Handler) Ask(w http.ResponseWriter, r *http.Request) {
    var req AskRequest
    if err := decodeJSON(r, &req); err != nil {
        writeError(w, r, apperr.BadRequest("invalid body", err)); return
    }
    if err := req.Validate(); err != nil {
        writeError(w, r, apperr.Validation(err)); return
    }
    resp, err := h.answers.Ask(r.Context(), req.toDomain())
    if err != nil {
        writeError(w, r, err); return
    }
    writeJSON(w, http.StatusOK, toAskResponse(resp))
}
```

DTOs live in `api` and are separate from domain types — never serialize domain
structs directly.

## Validation

Explicit `Validate() error` methods on request DTOs. Validate at the edge; assume
valid inside services. Bound everything user-controlled: query length, `top_k`
(cap it), page size, date ranges, uploaded file size.

## Errors

- Wrap with `%w` and context: `fmt.Errorf("retrieve chunks: %w", err)`.
- One `apperr` package mapping error kinds to HTTP status. Handlers never write
  status codes by inspecting error strings.
- `errors.Is` / `errors.As`, never string matching.
- Never leak SQL errors or provider errors to the client. Log the detail with a
  correlation ID, return a stable code and a safe message.
- `sql.ErrNoRows` / `pgx.ErrNoRows` becomes a domain not-found at the repository
  boundary.

## SSE streaming

The answer endpoint streams. Contract:

```
event: sources     data: {"sources":[{chunk_id, document_id, article_no, ...}]}
event: token       data: {"text":"..."}
event: citation    data: {"chunk_id":"...","offset":123}
event: done        data: {"answer_id":"...","usage":{...}}
event: error       data: {"code":"...","message":"..."}
```

Rules:

- Emit `sources` **before** the first token — the UI shows what grounds the
  answer while it generates.
- Set `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
  `X-Accel-Buffering: no`. Flush after every event via `http.Flusher`.
- Respect `r.Context()`: client disconnect must cancel the LLM call.
- Heartbeat comment (`: ping\n\n`) every ~15s to survive proxies.
- Errors mid-stream go out as an `error` event then close — you cannot change
  the status code after headers are sent.
- Persist the answer + citations server-side before `done`, so the client can
  reconnect and fetch it.

## Concurrency

- `context.Context` is the first parameter of every I/O function. Always with a
  timeout for external calls.
- Parallel fan-out (lexical + vector search, batch embeddings) uses
  `errgroup.WithContext`. Never a bare `go` in a request path.
- Bound concurrency with a semaphore or worker pool — never one goroutine per
  item over an unbounded slice.
- Guard shared maps; prefer passing values over sharing state.
- `go test -race` in CI, not optionally.

## pgx

- One `*pgxpool.Pool` from `main`, injected. Configure `MaxConns`, lifetimes,
  and a connect timeout.
- Always parameterized queries. String-built SQL is a defect; for dynamic
  filters build the WHERE clause with placeholders and an args slice.
- `pgx.CollectRows` + `pgx.RowToStructByName` for scanning.
- Transactions: `pgx.BeginFunc` so rollback can't be forgotten. Keep them short;
  never hold one across an LLM or HTTP call.
- Use `CopyFrom` for bulk chunk/embedding inserts.
- See `database` for schema and index rules.

## Config and logging

- Config from environment, parsed once into a struct, validated at startup;
  fail fast on missing required values. Secrets never in code or logs.
- `log/slog`, structured, JSON in production. Request ID in context and on every
  log line. Never log query text with PII, document contents, or API keys.

## Testing

Table-driven tests. Services tested against fakes; repositories tested against a
real Postgres+pgvector via testcontainers. See `testing-evaluation`.
