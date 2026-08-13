---
name: database
description: Use when designing or changing the Postgres schema, writing migrations, adding indexes, working with pgvector or full-text search, tuning queries, handling transactions, or modeling document versions for the Seera legal AI.
---

# Database (PostgreSQL + pgvector)

One Postgres instance with `pgvector` and `pg_trgm`. It stores documents,
versions, chunks, embeddings, and the search indexes. No separate vector
database (see `project-architecture`).

## Schema principles

- Legal data is **append-mostly**. Versions and chunks are inserted, superseded,
  and marked repealed — never updated in place or deleted.
- Identity vs. artifact: `documents` is the stable identity; `document_versions`
  is what gets parsed, chunked, embedded, and cited.
- Every chunk is independently citable: it can name its document, version,
  article, section path, page, and character span without a join to reconstruct
  meaning.
- Constraints in the database, not only in Go. Foreign keys, `NOT NULL`, checks,
  and unique constraints are correctness guarantees the application cannot skip.
- `TIMESTAMPTZ` always. `effective_from` / `repealed_at` are `DATE` where the
  law states a date, and they drive in-force filtering.
- UUIDs for surface IDs; `chunk_id` deterministic from content coordinates so
  re-ingestion is idempotent.

## Core tables

```sql
documents            -- identity: type, official number, issuing body
document_versions    -- one per ingested artifact: sha256, effective_from,
                     --   repealed_at, status, structure_confidence, ocr_used
chunks               -- version_id, ordinal, section_path, article_no,
                     --   page_no, char_start, char_end, text, text_search
embeddings           -- chunk_id, model, model_version, vector
document_relations   -- amends / repeals / cites edges between versions
ingestion_jobs       -- state machine, attempts, stage, last_error
answers, citations   -- persisted responses and their grounding chunks
```

`status` on `document_versions` gates visibility: retrieval only ever reads
`status = 'published'`. Building a version is invisible until the flip.

## Migrations

- Versioned, forward-only, checked in under `migrations/` in seera-backend-services
  (`goose` or `golang-migrate` — pick one and stay with it).
- One logical change per migration. Never edit a migration that has been
  applied anywhere; write a new one.
- Every migration has a tested `down`, even if `down` is "raise".
- **Expand → migrate → contract** for anything with live data: add nullable
  column, backfill in batches, add the constraint, then drop the old column in a
  later release.
- `CREATE INDEX CONCURRENTLY` on non-empty tables, and note that it cannot run
  inside a transaction — mark the migration accordingly.
- Backfills run in bounded batches with commits between, never one statement
  over the whole table.

## Full-text search

- Store a generated `tsvector` column, don't compute it per query:

```sql
ALTER TABLE chunks ADD COLUMN text_search tsvector
  GENERATED ALWAYS AS (to_tsvector('simple', coalesce(text, ''))) STORED;
CREATE INDEX chunks_fts_idx ON chunks USING GIN (text_search);
```

- Use the `simple` configuration for Amharic — English stemmers damage Ethiopic
  tokens. Keep a separate English-configured column if English recall needs
  stemming, and search both.
- `pg_trgm` GIN index on citation-bearing fields (`official_number`,
  `article_no`) so `Proclamation 1051/2017` and near-misses match.
- Rank with `ts_rank_cd`; feed the ranked list into RRF fusion rather than
  blending raw scores with vector distances (see `legal-rag`).

## Vector search

- Fixed dimension per embedding model; store `model` and `model_version` on
  every row. Mixed models in one index is a correctness bug, not a nuance.
- **HNSW** over IVFFlat for this workload — better recall/latency and no
  retraining as the corpus grows:

```sql
CREATE INDEX embeddings_hnsw_idx ON embeddings
  USING hnsw (vector vector_cosine_ops) WITH (m = 16, ef_construction = 64);
```

- Match the operator class to the distance used at query time (`<=>` cosine).
  A mismatch silently disables the index.
- Tune `hnsw.ef_search` per query (start 100); higher recall costs latency.
- Build the index **after** bulk loading a version's embeddings, not before.
- Filtered kNN: put selective predicates (version status, effective date) in the
  same query and verify the plan — heavy filtering plus HNSW can degrade recall,
  in which case over-fetch (`LIMIT k*4`) and filter, then re-limit.
- A model change is a re-embed migration into a new column/table plus an atomic
  swap — never in-place mutation of live vectors.

## Indexes to have

- `document_versions (document_id, effective_from DESC)`
- `document_versions (status) WHERE status = 'published'` (partial)
- `chunks (version_id, ordinal)`
- `chunks (article_no)` for direct citation lookup
- GIN on `text_search`, HNSW on `vector`, trigram on identifiers
- Every foreign key column used in joins

Add an index because a plan demands it; drop unused ones. Each index slows
ingestion writes.

## Transactions

- Short. Never hold a transaction across an LLM call, an embedding API call, or
  any network I/O.
- `pgx.BeginFunc` so rollback cannot be forgotten.
- Publishing a version — insert chunks, insert embeddings, flip status — is one
  transaction. Retrieval never sees a partial version.
- `READ COMMITTED` is the default and is right here; reach for
  `REPEATABLE READ` only with a specific anomaly in mind.
- Take locks in a consistent order across code paths.
- Bulk inserts via `CopyFrom`, not row-by-row loops.

## Query optimization

- `EXPLAIN (ANALYZE, BUFFERS)` before claiming a query is fast, and on realistic
  data volume — plans flip between 1k and 1M rows.
- Watch for: sequential scans on large tables, index scans discarded by a filter
  that should have been in the index, and row-estimate errors above ~10x.
- Keep `pg_stat_statements` on; optimize what the workload actually runs.
- No `SELECT *`. Retrieval must not pull full chunk text for candidates it will
  discard during fusion — fetch text only for the reranked survivors.
- Keyset pagination for document lists, not `OFFSET` at depth.
- `ANALYZE` after bulk ingestion; autovacuum tuning matters on the chunk table.

## Never

- String-concatenated SQL. Always parameterized.
- Deleting or overwriting a document version.
- Storing legal text only in application code or prompts.
- Application-level "soft delete" that retrieval forgets to filter — use the
  `published` partial index and a single repository function that owns the
  visibility predicate.
