---
name: legal-rag
description: Use when working on retrieval, hybrid search, embeddings, chunking strategy, reranking, prompt construction, grounding, citations, or hallucination prevention for the Seera legal AI. Load before touching anything in the retrieval or answer path.
---

# Legal RAG

Legal QA is a high-precision, low-tolerance retrieval problem. A confidently
wrong answer about a statute is worse than no answer. Optimize for grounding
and citation accuracy before fluency.

## Pipeline

```
query
 → normalize + expand (Amharic/English, legal synonyms)
 → hybrid retrieve: BM25/FTS (top 50) ∪ vector kNN (top 50)
 → fuse (Reciprocal Rank Fusion)
 → filter (jurisdiction, doc type, in-force-at-date)  ← deterministic Go, not the LLM
 → rerank (cross-encoder) → top 5–8
 → build context with chunk IDs
 → generate with citation-required prompt
 → validate citations against retrieved set
 → stream
```

## Retrieval rules

- **Hybrid always.** Legal queries mix exact terms ("Proclamation No. 1051/2017",
  "Article 27(2)") with paraphrase. Vector-only misses citations; lexical-only
  misses intent. Fuse with RRF (`k=60`) rather than tuning score weights.
- **Filters are SQL predicates**, applied in the query, not post-hoc in Go and
  never delegated to the model. In-force filtering: `effective_from <= $date AND
  (repealed_at IS NULL OR repealed_at > $date)`.
- **Rerank before generation.** Retrieve wide (50–100), rerank, pass few (5–8).
  Passing 50 chunks degrades grounding and cost.
- **Never drop the metadata.** Each retrieved chunk carries `chunk_id`,
  `document_id`, `document_title`, `article_no`, `section_path`, `version_id`,
  `effective_from`. The generator sees them; the response carries them back.

## Chunking

- Chunk on **legal structure first**: article → sub-article → paragraph. A chunk
  should be a semantically complete provision, not an N-token window.
- Fall back to ~512-token windows with ~15% overlap only when structure
  detection fails, and mark the chunk `structure_confidence = low`.
- **Prepend context** to each chunk's embedded text: document title, chapter,
  article number. An isolated "(2) The period shall be thirty days." is
  unretrievable without it.
- Store the raw span (`char_start`, `char_end`, `page_no`) so the UI can
  highlight the source in the original document.
- See `document-ingestion` for structure detection.

## Embeddings

- Behind an interface: `embed.Embedder{ Embed(ctx, []string) ([][]float32, error) }`.
- Store `embedding_model` and `embedding_version` on every vector row. A model
  change means a re-embed migration, not mixed vectors in one index.
- Embed the contextualized chunk text, not the raw span.
- Amharic matters: verify any candidate model's Amharic performance on a held-out
  set before adopting it. Do not assume multilingual claims hold.

## Generation and grounding

Prompt contract:

- System prompt states: answer **only** from the provided sources; cite every
  claim as `[chunk_id]`; if the sources are insufficient, say so explicitly.
- Sources block: numbered chunks with their metadata header.
- Never include legal text in the system prompt itself — it comes from retrieval.
- Instruct the model to distinguish *what the law says* from *how it is applied*,
  and to never give individual legal advice.

Post-generation validation (Go, not optional):

1. Parse citation markers from the answer.
2. Every marker must resolve to a chunk in this request's retrieved set.
   Unknown marker → strip the claim or fail the response; log it.
3. An answer with zero citations and non-empty legal content is a bug — return
   the abstain response instead.
4. Attach the resolved citation objects to the response payload; the frontend
   renders from those, never from parsing the answer text.

## Abstention

Abstain when: fused top score below threshold, reranker top score below
threshold, or retrieved chunks all fail the in-force filter. The abstain
response names what was searched and suggests a narrowing — it does not
apologize and then guess.

## Evaluation

Every change to chunking, embeddings, fusion, or reranking must be measured, not
argued. Precision@5, citation accuracy, groundedness, abstention correctness.
See `testing-evaluation`.

## Anti-patterns

- Letting the LLM decide jurisdiction or effective date.
- Free-text citations ("Article 5 of the Civil Code") without a `chunk_id`.
- Summarizing retrieved chunks into a cache and retrieving over summaries.
- Raising `top_k` to fix a recall problem caused by bad chunking.
- Adding a "general knowledge" fallback path when retrieval is empty.
