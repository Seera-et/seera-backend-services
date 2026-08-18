-- Embeddings live in one table per embedding space, not one shared table with a
-- model column.
--
-- Two reasons. Dimensions are fixed per vector column, and different models
-- have different dimensions. And cosine distance between vectors from two
-- different models is meaningless — mixing them in one index produces confident
-- nonsense rather than an error. Adopting a second model means adding a table
-- and backfilling it, which also allows both to be indexed at once for the
-- language-split A/B the plan calls for.

CREATE TABLE chunk_embeddings_openai_1536 (
    chunk_id      uuid PRIMARY KEY REFERENCES chunks (id) ON DELETE CASCADE,
    embedding     vector(1536) NOT NULL,
    -- Recorded per row so a partially re-embedded corpus is detectable rather
    -- than silently mixed.
    model         text        NOT NULL,
    model_version text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- HNSW over cosine distance. 1536 dimensions is chosen partly because pgvector
-- cannot build an HNSW index on a `vector` wider than 2000.
CREATE INDEX chunk_embeddings_openai_1536_hnsw_idx
    ON chunk_embeddings_openai_1536
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
