-- The legal corpus: documents, their versions, the articles inside a version,
-- and the chunks retrieval runs over.
--
-- The central invariant is that legal history is never destroyed. An amendment
-- inserts a new document_versions row and marks the old one superseded; no row
-- here is ever rewritten in place.

CREATE TABLE legal_documents (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title             text        NOT NULL,
    doc_type          text        NOT NULL,
    legal_domain      text        NOT NULL DEFAULT '',
    issuing_authority text        NOT NULL DEFAULT '',
    publication_date  date,
    effective_date    date,
    language          text        NOT NULL,
    -- Official publication location, kept for verification only. Citations
    -- never resolve through it.
    source_url        text        NOT NULL DEFAULT '',
    -- Key of the original PDF in object storage.
    storage_key       text        NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT legal_documents_language_check
        CHECK (language IN ('en', 'am')),
    CONSTRAINT legal_documents_doc_type_check
        CHECK (doc_type IN ('proclamation', 'regulation', 'directive', 'code', 'other'))
);

-- Guards against ingesting the same document twice, which would double every
-- retrieval result for it.
CREATE UNIQUE INDEX legal_documents_title_language_key
    ON legal_documents (lower(title), language);

CREATE TABLE document_versions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id    uuid        NOT NULL REFERENCES legal_documents (id) ON DELETE CASCADE,
    version_label  text        NOT NULL,
    effective_from date,
    repealed_at    date,
    status         text        NOT NULL DEFAULT 'draft',
    published_at   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT document_versions_status_check
        CHECK (status IN ('draft', 'published', 'superseded')),
    CONSTRAINT document_versions_label_key
        UNIQUE (document_id, version_label)
);

-- At most one live version per document. A new publication supersedes the old
-- one in the same transaction; the database refuses to hold two at once.
CREATE UNIQUE INDEX document_versions_one_published_per_document
    ON document_versions (document_id)
    WHERE status = 'published';

CREATE INDEX document_versions_status_idx ON document_versions (status);

CREATE TABLE articles (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version_id  uuid NOT NULL REFERENCES document_versions (id) ON DELETE CASCADE,
    document_id uuid NOT NULL REFERENCES legal_documents (id) ON DELETE CASCADE,

    -- article_no is normalised to Arabic numerals; article_no_raw preserves the
    -- printed form, which in Amharic documents is often Ge'ez ("፵፭"). Both are
    -- stored so a user searching either way finds the article.
    article_no     text NOT NULL,
    article_no_raw text NOT NULL DEFAULT '',

    title        text   NOT NULL DEFAULT '',
    chapter      text   NOT NULL DEFAULT '',
    section_path text[] NOT NULL DEFAULT '{}',
    text         text   NOT NULL,
    ordinal      int    NOT NULL,

    -- Span in the extracted source text, so the UI can highlight the provision
    -- in the original document.
    page_no    int NOT NULL DEFAULT 0,
    char_start int NOT NULL DEFAULT 0,
    char_end   int NOT NULL DEFAULT 0,

    -- 'low' means article detection failed and the text was cut into fixed-size
    -- windows. Carried to retrieval so degraded chunks can be measured rather
    -- than quietly polluting results.
    structure_confidence text NOT NULL DEFAULT 'high',

    CONSTRAINT articles_structure_confidence_check
        CHECK (structure_confidence IN ('high', 'low')),
    CONSTRAINT articles_ordinal_key UNIQUE (version_id, ordinal)
);

CREATE INDEX articles_version_article_no_idx ON articles (version_id, article_no);
CREATE INDEX articles_document_idx ON articles (document_id);

CREATE TABLE chunks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    article_id  uuid NOT NULL REFERENCES articles (id) ON DELETE CASCADE,
    version_id  uuid NOT NULL REFERENCES document_versions (id) ON DELETE CASCADE,
    document_id uuid NOT NULL REFERENCES legal_documents (id) ON DELETE CASCADE,
    language    text NOT NULL,

    -- text is the raw span and is what the user is shown. embed_text is the
    -- contextualised form that is embedded and full-text indexed: document
    -- title, chapter, and article number prepended to the span. Without that
    -- header an isolated "(2) The period shall be thirty days." is
    -- unretrievable, because nothing in it says which period, under which law.
    text       text NOT NULL,
    embed_text text NOT NULL,

    token_count int NOT NULL DEFAULT 0,
    ordinal     int NOT NULL,

    -- Postgres ships no Amharic dictionary — no stemmer, no stop words — so
    -- Amharic indexes under 'simple' and leans on the trigram index below for
    -- fuzziness. This mapping is duplicated in Go (legal.Language.FTSConfig),
    -- which builds the query side; the two must agree or lexemes will not match.
    fts tsvector GENERATED ALWAYS AS (
        to_tsvector(
            CASE WHEN language = 'en' THEN 'english'::regconfig ELSE 'simple'::regconfig END,
            embed_text
        )
    ) STORED,

    CONSTRAINT chunks_language_check CHECK (language IN ('en', 'am')),
    CONSTRAINT chunks_ordinal_key UNIQUE (article_id, ordinal)
);

CREATE INDEX chunks_fts_idx ON chunks USING gin (fts);
CREATE INDEX chunks_text_trgm_idx ON chunks USING gin (text gin_trgm_ops);
CREATE INDEX chunks_version_idx ON chunks (version_id);
CREATE INDEX chunks_document_idx ON chunks (document_id);
CREATE INDEX chunks_article_idx ON chunks (article_id);
