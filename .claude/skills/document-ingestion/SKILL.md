---
name: document-ingestion
description: Use when working on the ingestion pipeline — PDF/DOCX parsing, PyMuPDF, OCR, article and section detection, legal document structure, chunk production, metadata extraction, ingestion job orchestration, document versioning, or handling documents that fail to parse.
---

# Document Ingestion

Python workers under `workers/` in seera-backend-services — the only place Python runs (see
`project-architecture`). They turn source documents into structured, versioned,
chunked, cited-able rows. Everything downstream inherits their mistakes, so
this pipeline fails loudly rather than producing plausible garbage.

## Pipeline

```
upload / crawl
  → store raw file + sha256 (object storage; never mutate)
  → detect type + text layer
  → extract (PyMuPDF | python-docx | OCR)
  → normalize text (Ethiopic + Latin)
  → detect structure (chapter / article / sub-article / paragraph)
  → extract metadata (proclamation no., title, gazette date, effective date)
  → resolve version (new doc | new version | amendment)
  → chunk on structure
  → embed
  → persist (transactional)
  → validate + report
```

Each stage writes its status to the job row. A failure never leaves partial
chunks visible to retrieval.

## Extraction

- **PyMuPDF (`fitz`)** is the default. Extract with
  `page.get_text("dict")` — not `"text"` — so you keep block/line/span geometry,
  font size, and bold flags. Structure detection depends on that geometry.
- Record `page_no`, and `char_start`/`char_end` offsets into the normalized
  document text for every span. The viewer highlight and the citation both
  depend on these; a chunk without offsets is not shippable.
- Detect two-column and table layouts before reading order is assumed. Sort
  blocks by column then vertical position, not raw y.
- Drop repeating headers/footers by finding blocks that recur at the same
  geometry across pages — not by a fixed line count.
- DOCX: `python-docx`, using paragraph styles as structure hints (a real
  "Heading 2" beats a regex).

## OCR

Trigger OCR when a page yields near-zero extractable text or the text layer is
garbage (high ratio of replacement chars / no dictionary hits).

- Tesseract with `amh+eng`. Verify the Amharic traineddata is installed —
  Ethiopic without it silently produces noise.
- Preprocess: deskew, denoise, adaptive threshold, ≥300 DPI render.
- Store `ocr_used = true` and mean per-page confidence on the document version.
- Pages below a confidence floor are flagged for human review and their chunks
  are marked `low_confidence` — retrievable only when explicitly allowed, and
  never used as sole grounding for an answer.
- OCR output keeps page-level offsets only; be honest that char offsets are
  approximate and let the viewer highlight the page rather than a fake span.

## Structure detection

Ethiopian legal documents are regular enough to parse deterministically. Use
rules first, and only consider a model when rules measurably fail.

Detect, in both Amharic and English:

- Proclamation / Regulation / Directive number and year
  (`Proclamation No. 1051/2017`, `አዋጅ ቁጥር ፲፻፶፩/፳፻፱`)
- `PART`, `CHAPTER`, `SECTION` headings (`ክፍል`, `ምዕራፍ`)
- `Article N` / `አንቀጽ N` — the primary unit
- Sub-articles `(1)`, `(2)`; paragraphs `(a)`, `(b)`; and Ethiopic numerals
- Schedules, annexes, transitional provisions, repeal clauses

Rules:

- Normalize Ethiopic numerals to integers but **keep the original string** for
  display and citation.
- Combine signals: regex match **and** geometry (font size, boldness, indent,
  line-alone). A regex-only match inside a body paragraph is a false heading.
- Build a tree, not a flat list. Every chunk stores its `section_path`
  (`Chapter 3 > Article 27 > (2)`).
- Emit `structure_confidence` per document. Below threshold → quarantine for
  review rather than ingest.
- Unit-test detection against fixture pages from real documents; a regression
  here silently corrupts citations. See `testing-evaluation`.

## Metadata

Per document version, extracted and stored explicitly: title (am + en),
document type, official number, gazette publication date, `effective_from`,
`repealed_at`, issuing body, language(s), source URL/provenance, sha256, page
count, `ocr_used`, `structure_confidence`.

Never infer `effective_from` from a filename. If it isn't in the text, leave it
null and flag the document — a wrong effective date makes in-force filtering
return wrong law.

## Chunking

Structure-aware, as specified in `legal-rag`:

- One chunk per article, or per sub-article when the article is long.
- Never split mid-sentence or across an article boundary.
- Prepend the contextual header (document title, chapter, article number) to the
  embedded text; store it separately from the display text.
- Carry `document_id`, `version_id`, `section_path`, `article_no`, `page_no`,
  `char_start`, `char_end`, `language`, `confidence` on every chunk.
- Deterministic `chunk_id` (hash of `version_id` + `section_path` + ordinal) so
  re-ingestion is idempotent and citations stay stable.

## Versioning

- A document is an identity; a **version** is the ingestible artifact. Chunks
  and embeddings belong to a version.
- Re-ingesting identical content (same sha256) is a no-op, not a new version.
- Amendments create a new version of the amended document with new
  `effective_from`, and record an `amends` edge to the amending instrument.
  Prior versions stay queryable and stay in the index.
- Repeal sets `repealed_at`; it never deletes rows.
- Publishing is atomic: build the new version's chunks and embeddings first,
  then flip it live in one transaction. Retrieval must never see a half-indexed
  version.
- See `database` for the schema.

## Jobs

- Every ingestion is a row in `ingestion_jobs`: state machine
  (`queued → extracting → parsing → chunking → embedding → completed | failed |
  quarantined`), with `attempts`, `last_error`, `stage`, timestamps.
- Idempotent by `(source_hash, pipeline_version)`. Bump `pipeline_version` when
  parsing logic changes so documents can be reprocessed intentionally.
- Retry only transient failures (network, provider 5xx/429) with exponential
  backoff. Parse failures are not retried — they are quarantined.
- Bounded worker concurrency; stream page-by-page rather than loading whole
  large PDFs into memory.
- Log per-stage timings and counts. A drop in chunks-per-page is the earliest
  signal that a parser regressed.

## Failed documents

Failure is normal at corpus scale and must be visible:

- `quarantined` documents are stored with the reason, the stage, and the raw
  file retained — never silently dropped.
- An admin surface lists quarantined documents with the failure reason and
  supports re-running a single document after a fix.
- Partial success is not published. A document with 3 of 40 articles parsed is
  quarantined, because retrieval over it produces confidently incomplete
  answers.
- Report ingestion coverage (documents ingested / attempted, chunks per
  document, OCR rate, quarantine rate) as a first-class metric.
