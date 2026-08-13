---
name: project-architecture
description: Load before any non-trivial change to the Seera Ethiopian legal AI system — adding a dependency, creating a package/service, choosing where logic lives, or wiring frontend to backend. Defines the fixed architecture and the rules Claude must not break.
---

# Seera — Project Architecture

Ethiopian legal AI: a grounded question-answering system over Ethiopian legal
documents (proclamations, regulations, directives, codes, case law).

## The stack is decided

```
React 19 + TypeScript + Vite      (seera-web)
            │  HTTP / JSON + SSE
            ▼
Go REST API                        (seera-backend-services)
            │
        Services                   (retrieval, answer, ingestion, auth)
            │
PostgreSQL + pgvector
```

Python appears **only** in document/ML processing workers (PDF parsing, OCR,
layout detection, embedding batch jobs). It is a worker runtime, never the API.

Repo layout (two working directories on this machine):

- `seera-web/` — frontend
- `seera-backend-services/` — Go API, services, migrations, and `workers/` for Python

## Hard rules

Architecture:

1. **No Next.js.** No SSR framework, no React Server Components, no file-system
   routing framework. Vite SPA only.
2. **No Python as the main backend.** FastAPI/Django/Flask are not options for
   the API. Python is confined to `workers/`.
3. **No new microservices** without a written justification of the scaling or
   isolation problem being solved. Start as one Go binary with internal packages.
4. **No ORM magic.** `pgx` with hand-written SQL. No GORM/ent.
5. **No architecture change without explanation.** If a task seems to require
   breaking a rule above, stop and explain the tradeoff before writing code.

Legal correctness:

6. **Never bypass the RAG layer for legal answers.** Every legal response goes
   through retrieval. No direct LLM calls that answer legal questions from
   parametric memory.
7. **Every legal claim must be grounded** in retrieved chunks that are part of
   the same request's context. If retrieval returns nothing relevant, the correct
   answer is "I could not find this in the corpus" — not a plausible guess.
8. **Citations reference internal database records** (`document_id`,
   `chunk_id`, article/section number, version). Never a URL, never a
   model-generated citation string, never a page number the parser didn't emit.
9. **Never hardcode legal information in application code.** No article text,
   no penalty amounts, no deadlines, no legal thresholds in Go/TS source or
   prompts. It lives in the database or it does not exist.
10. **Preserve legal document versions.** Amendments and repeals create new
    versions; they never overwrite or delete prior rows. An answer must be able
    to state which version it relied on and its effective date.

Engineering:

11. **Business rules stay out of LLM reasoning.** Access control, jurisdiction
    filters, date-effectiveness filters, and quotas are deterministic Go code —
    not prompt instructions the model may ignore.
12. **LLM and embedding providers sit behind Go interfaces** (`llm.Provider`,
    `embed.Embedder`). No vendor SDK types cross a service boundary. Swapping
    providers must be a wiring change.
13. **Prefer simple until complexity is justified.** No queues, caches, feature
    flags, or abstraction layers added speculatively.
14. **Tests for important logic**: retrieval, chunking, citation mapping,
    versioning, auth. See `testing-evaluation`.

## Where things go

| Concern | Location |
|---|---|
| HTTP handlers, DTOs, SSE | `internal/api/` |
| Retrieval, answering, ingestion orchestration | `internal/service/` |
| SQL, pgx queries | `internal/repository/` |
| Domain types, interfaces | `internal/domain/` |
| LLM/embedding adapters | `internal/provider/` |
| Migrations | `migrations/` in seera-backend-services |
| PDF/OCR/parsing | `workers/` (Python) |
| UI | `seera-web/src/` |

## Related skills

`legal-rag`, `backend-go`, `frontend-react`, `document-ingestion`, `database`,
`testing-evaluation`. Read the specific one before working in that layer.
