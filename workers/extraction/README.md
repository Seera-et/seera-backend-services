# Document extraction worker

Offline Python that turns a legal PDF into structured JSON. It is **not a service**: no
HTTP, no queue, no deployment. You run it by hand over a handful of documents, read its
report, and hand the JSON to the Go ingestion endpoint.

Python lives here and nowhere else in this repo. The API is Go.

## Why offline

At 10–20 documents, parse quality is won by looking at the output. A long-lived worker
buys you nothing at this size and costs a second runtime, a second deploy target, and a
second failure mode before a single document is indexed. If the corpus grows to the point
where a human can no longer inspect each parse, revisit that decision then.

## Triage first

Before extracting anything, assess the folder:

```bash
python triage.py "C:/Users/PC/Documents/ethiopian law" --json triage.json
```

It classifies every PDF as `native`, `junk`, `legacy`, `partial`, `scanned` or `damaged`,
flags byte-identical duplicates, and prints the subset that is ingestible today. Findings
for the current corpus are written up in [`docs/CORPUS.md`](../../docs/CORPUS.md).

`junk` is the category that matters: a PDF can carry a *complete* text layer that is pure
OCR debris, and it looks healthy by page count, file size and character count. Ingesting
it is worse than ingesting nothing, because the chunks embed to noise and then come back
as sources for real legal questions. `extract.py` refuses those documents outright rather
than warning about them.

## Setup

```bash
cd workers/extraction
python -m venv ../.venv
source ../.venv/bin/activate      # Windows: ../.venv/Scripts/activate
pip install -r requirements.txt
```

OCR additionally needs the Tesseract binary and its language data on `PATH` —
`eng` and `amh`. It is only invoked for pages whose text layer is empty.

## Running it

```bash
python extract.py commercial_code.pdf --language en --out out/
python extract.py labor_proclamation.pdf --language am --out out/ --ocr
```

Exit status is `2` when a document fell back to window chunking, so a batch run
cannot quietly produce a corpus that is mostly unstructured.

### Over a crawler store

`--store-root` writes each document to `<root>/structured/<doc-id>/document.json`,
taking the id from the PDF's parent directory. Use it for anything the crawler
acquired, because every file it downloads is named `original.pdf` — with `--out`
a batch run writes all of them to `out/original.json`, keeping only the last and
saying nothing about the rest.

```bash
python extract.py ../../data/raw/*/original.pdf --language en --store-root ../../data
```

Then `cmd/ingest` takes it from there: it reads these files alongside the crawl
records, builds each document's metadata from the record rather than asking for
it again, and publishes what clears its quality gates.

```bash
go run ./cmd/ingest -data ./data -dry-run
go run ./cmd/ingest -data ./data
```

### One document at a time

For a single file outside a crawl, the API endpoint still takes the JSON
directly:

```bash
jq -n --arg t "Commercial Code of Ethiopia" --slurpfile e out/commercial_code.json '{
  document: {title: $t, doc_type: "code", language: "en",
             issuing_authority: "FDRE", publication_date: "2021-03-25"},
  version:  {label: "2021", effective_from: "2021-04-01"},
  publish:  false,
  extraction: $e[0]
}' | curl -sS -X POST http://localhost:8080/api/v1/admin/documents/ingest \
       -H "Authorization: Bearer $ADMIN_API_TOKEN" \
       -H "Content-Type: application/json" --data-binary @-
```

`publish` defaults to false on purpose: a document is invisible to retrieval
until someone has looked at how it parsed. Re-send with `"publish": true` — or
ingest a corrected version — once the parse looks right.

## Tests

```bash
python -m unittest discover -s workers/extraction
```

Numeral conversion and article detection are tested because a mistake in either
produces a corpus that looks fine and cites the wrong law. PDF handling is not:
it needs real documents, and its failure mode is visible in the report.

## Output contract

One JSON file per PDF, written to `out/` (gitignored — legal source material and its
derivatives are never committed). This shape is what `internal/ingestion/parser.go` reads;
changing it means changing both sides.

```json
{
  "source_file": "commercial_code_2021.pdf",
  "language": "en",
  "page_count": 412,
  "articles": [
    {
      "article_no": "45",
      "article_no_raw": "45",
      "title": "Formation of private limited companies",
      "chapter": "Companies",
      "section_path": ["Book II", "Title V", "Chapter 3"],
      "text": "A private limited company may be formed by...",
      "page_no": 88,
      "char_start": 145203,
      "char_end": 147891,
      "structure_confidence": "high"
    }
  ]
}
```

Field notes:

- `article_no` is normalised to Arabic numerals. `article_no_raw` preserves the printed
  form, which in Amharic documents is often Ge'ez (`፵፭`). Both are stored, so a user
  searching either way finds the article.
- `structure_confidence` is `"high"` when the article was recovered from the document's
  real structure and `"low"` when detection failed and the text was cut into fixed-size
  windows instead. Low-confidence chunks are ingested but measured separately.
- `char_start` / `char_end` index into the document's extracted text, so the UI can
  highlight the exact span in the source.

`stats` carries the numbers `cmd/ingest`'s quality gates read. They are measured
here because this is the only place that sees the document's text, and they are
reported as numbers rather than as warning prose so the gates do not have to
pattern-match English sentences:

| Key | Gate it feeds |
|---|---|
| `pages_without_text_layer` | a scan with a few incidental text pages, which passes every other check |
| `article_characters`, `characters` | how much of the text actually landed inside a provision |
| `article_gaps`, `article_number_min`/`max` | markers the extractor missed, with the range so a misread page number is visible as such |
| `ethiopic_ratio` | the `--language` you passed against the script actually present |
| `text_quality` | reported only; extraction already refuses below 0.60 |

New keys go **inside `stats`**. The Go side decodes the top level with
`DisallowUnknownFields`, so a new top-level key breaks every existing caller,
while `stats` is a free-form map.

## Per-document report

Every run prints, per document: articles detected, pages with an empty text layer, pages
sent to OCR, and whether the run fell back to window chunking. Read it before ingesting.
A document that silently produced 4 articles from 400 pages will not announce itself later
— it will just quietly fail to answer questions.

## Known limits

- **Structure detection is pattern-based.** It knows the heading forms used in
  Ethiopian proclamations, regulations and codes (`ARTICLE`/`Art.`/`አንቀጽ`,
  `BOOK`/`PART`/`TITLE`/`CHAPTER`/`SECTION`, `ክፍል`/`ምዕራፍ`). A document that
  numbers its provisions some other way falls back to windows and says so.
- **Article titles are only captured when they share a line with the number.**
  Below an article marker is the provision's body, and guessing that its first
  line is a title would put body text into every citation. Chapter headings do
  get a one-line lookahead, because there the next line is reliably the title.
- **Ge'ez numerals are converted up to 9,999** — far beyond any real article
  number. Anything the converter does not recognise is passed through unchanged
  rather than guessed at, so it reaches a human instead of becoming a wrong
  citation.
