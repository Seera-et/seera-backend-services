-- A second embedding space, alongside the OpenAI one rather than replacing it.
--
-- This is the migration path the plan reserved for exactly this moment: the
-- corpus turned out to be mostly Amharic-adjacent and Gemini's multilingual
-- coverage is materially better than text-embedding-3-large's, so the embedder
-- changed. Both tables can be populated at once, which is what makes an
-- A/B on the language-split Precision@5 gate possible instead of theoretical.

CREATE TABLE chunk_embeddings_gemini_1536 (
    chunk_id      uuid PRIMARY KEY REFERENCES chunks (id) ON DELETE CASCADE,
    embedding     vector(1536) NOT NULL,
    model         text        NOT NULL,
    model_version text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX chunk_embeddings_gemini_1536_hnsw_idx
    ON chunk_embeddings_gemini_1536
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
