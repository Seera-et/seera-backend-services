-- Extensions the retrieval layer depends on.
--
-- vector    — embedding storage and cosine kNN (pgvector).
-- pg_trgm   — trigram similarity. This is what carries Amharic fuzzy matching:
--             Postgres ships no Amharic dictionary, so Amharic text is indexed
--             under the "simple" configuration and cannot rely on stemming.
-- unaccent  — normalises accented Latin characters in English queries.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;
