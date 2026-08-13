---
name: testing-evaluation
description: Use when writing or reviewing tests for Seera — Go unit and integration tests, repository tests against Postgres, API tests, React component tests — or when building RAG evaluation: Precision@5, citation accuracy, groundedness, abstention, latency, and evaluation datasets.
---

# Testing & Evaluation

Two distinct disciplines. **Tests** assert deterministic behavior and must pass.
**Evaluations** measure a probabilistic system and produce scores that are
compared against a baseline. Never mix them in the same suite: a flaky eval in
CI trains everyone to ignore red.

## What must be tested

Non-negotiable coverage:

- Structure detection (article/section parsing)
- Chunking boundaries and deterministic `chunk_id`
- Citation resolution and the post-generation validator
- Version resolution and in-force date filtering
- Hybrid fusion (RRF) ranking math
- Auth, access control, quotas
- SSE framing and cancellation

Not worth testing: getters, DTO mapping with no logic, framework behavior.

## Go tests

- Table-driven, `t.Run` per case, `t.Parallel()` where safe.
- `go test -race ./...` in CI.
- Services tested against hand-written fakes implementing the consumer's
  interface. No mocking framework, no generated mocks.
- Deterministic fake LLM and fake embedder: fixed vectors, scripted responses.
  Real providers never appear in tests.
- Golden files for prompt construction and SSE output — a prompt change should
  show up as a reviewable diff.
- Assert on error *kinds* via `errors.Is`/`errors.As`, never on message strings.
- Inject clocks; no `time.Now()` in testable code paths.

```go
tests := []struct {
    name    string
    input   domain.Query
    chunks  []domain.Chunk
    want    []string
    wantErr error
}{ ... }
```

## Repository / integration tests

- Real Postgres with pgvector via testcontainers. Never sqlite, never a mock
  database — the point is to test SQL, indexes, and pgvector behavior.
- Run migrations against the container at suite start; that also tests the
  migrations.
- Each test in a transaction rolled back at the end, or a truncate between
  tests. Tests must not depend on ordering.
- Small deterministic fixture corpus (a handful of real Ethiopian documents,
  checked in) shared by repository and evaluation suites.
- Build tag or `-short` guard so unit tests stay fast locally:
  `//go:build integration`.
- Assert index usage on the hot retrieval query with `EXPLAIN` where a
  regression would be silent.

## API tests

- Exercise the real router via `httptest.NewServer` with fakes below the
  service layer.
- Cover: happy path, validation failures, auth failures, not-found, oversized
  payload, out-of-range `top_k`.
- Assert status codes, stable error codes, and response shape — not prose.
- SSE tests: consume the stream and assert event order (`sources` before the
  first `token`, `done` last), that client cancellation stops generation, and
  that a mid-stream error emits an `error` event rather than changing status.
- Assert no internal error text, SQL, or provider detail leaks into responses.

## Frontend tests

- Vitest + React Testing Library. Query by role and accessible name — that
  tests accessibility and behavior at once.
- MSW for API mocking, including a mocked SSE stream.
- Cover: the four async states (idle/loading/error/empty), citation click →
  viewer navigation, abstention rendering, cancel mid-stream, and stream
  reconnect.
- No snapshot tests of whole trees.

## RAG evaluation

Runs as a separate command against a fixed dataset, on demand and nightly — not
in the PR test suite. Every change to chunking, embeddings, fusion, reranking,
or prompts must report before/after numbers.

### Dataset

Checked into `eval/datasets/`, versioned, built from real questions:

```json
{
  "id": "emp-notice-period-001",
  "question": "How much notice must an employer give before termination?",
  "question_am": "...",
  "relevant_chunk_ids": ["..."],
  "relevant_articles": ["Proclamation 1156/2019 Art. 35"],
  "expected_behavior": "answer",
  "as_of_date": "2026-01-01",
  "notes": "post-2019 version only; 2003 version is repealed"
}
```

Include, deliberately:

- Amharic and English phrasings of the same question
- Exact-citation lookups ("what does Article 27(2) say")
- Questions whose answer changed across versions (tests in-force filtering)
- **Out-of-corpus questions** with `expected_behavior: "abstain"` — a system
  that never abstains is not safe, and only these cases catch it
- Adversarial premises ("which article bans X" where no such article exists)

Ground truth is labeled by a person who checked the source. Never label the
gold set with the same model that generates answers.

### Metrics

Retrieval:

- **Precision@5** — fraction of the top 5 that are genuinely relevant. Primary
  gate; legal answers use few chunks, so the top 5 is what matters.
- **Recall@50** — did the candidate stage find it at all? Separates a retrieval
  bug from a reranking bug.
- **MRR / nDCG@10** — rank quality.

Answer:

- **Citation accuracy** — every citation resolves to a real chunk in the
  request's retrieved set *and* supports the sentence it is attached to.
  Fabricated or mismatched citations are a hard failure, target 100%.
- **Groundedness** — share of claims entailed by the cited chunks. Judged by an
  LLM judge with the chunk text in hand, spot-audited by a human each cycle.
- **Abstention correctness** — abstained when it should (recall on the
  out-of-corpus set) and did not abstain when it shouldn't.
- **Version correctness** — cited the version in force at `as_of_date`.

Latency, measured p50/p95 per stage (retrieval, rerank, time-to-first-token,
total) — never as a mean.

### Running it

- Fixed seeds, temperature 0 for judged runs, pinned model versions. Record
  model ids, `pipeline_version`, and dataset version with every result.
- Output a comparison table against the stored baseline, with per-question
  regressions listed — an aggregate that holds steady while five questions
  break is a regression.
- Store results in `eval/results/` so trends are reviewable.
- Gate merges on: citation accuracy no worse than baseline, Precision@5 not down
  more than a stated tolerance, abstention correctness not down.

### Judge rules

- The judge sees the question, the answer, and the cited chunks — nothing else.
  Never its own prior verdict, never the "expected answer" prose.
- Judge one dimension per call.
- Validate the judge against human labels on a sample before trusting it, and
  re-validate when the judge model changes.
