-- Provenance on the version, not the document.
--
-- Until now a version could not say which file produced it. `source_url` and
-- `storage_key` live on legal_documents, and ingestion.Service.resolveDocument
-- reuses an existing document row *without updating it* — so when a second
-- version attaches to a document, the new file's URL and storage key are
-- silently discarded and the only pointer back to a PDF is the first version's.
-- Provenance is a property of the artifact that was parsed, which is the
-- version.
--
-- source_checksum is the sha256 of that file. The crawler has computed it since
-- it was written (internal/crawler/downloader.go) and kept it in
-- data/index/checksums.json — a file on one operator's disk, which means the
-- database has never been able to answer "have I already ingested these bytes".
-- It now can, and that answer is worth real money: ingestion embeds before it
-- opens its transaction, so without this check a re-run of an already-ingested
-- document pays the full embedding cost (minutes, and a meaningful slice of a
-- free-tier daily quota) before failing on the version label.
--
-- The unique index is GLOBAL, not (document_id, source_checksum). A
-- document-scoped unique would miss the case that actually happens: the
-- Ministry edits a law page's title between crawls, FindDocumentByTitle misses,
-- a second legal_documents row is created, and the identical PDF is ingested
-- again under a new identity — doubling that law in every retrieval result. A
-- global unique says "these exact bytes are already in the corpus somewhere",
-- which is precisely the claim we want to make. It does not constrain a
-- document from having many versions: different editions are different bytes.
--
-- Partial, so the existing rows (and any version ingested from something that
-- is not a file) stay legal. Nullable rather than NOT NULL DEFAULT '' for the
-- same reason: "unknown" and "empty" are different, and only NULL is excluded
-- from the index.
--
-- Not CONCURRENTLY — the runner applies one migration per transaction under an
-- advisory lock, and CREATE INDEX CONCURRENTLY cannot run inside one.
ALTER TABLE document_versions
    ADD COLUMN source_checksum text,
    ADD COLUMN source_url      text NOT NULL DEFAULT '',
    ADD COLUMN storage_key     text NOT NULL DEFAULT '';

CREATE UNIQUE INDEX document_versions_source_checksum_key
    ON document_versions (source_checksum)
    WHERE source_checksum IS NOT NULL;
