# Ministry of Justice crawler

Acquires Ethiopian legal documents from [justice.gov.et](https://justice.gov.et/en/):
discover law pages → find the real PDF behind each → download → validate → checksum → record.

It stops at a validated PDF plus a metadata record. Text extraction, OCR and
structure detection are `workers/extraction`'s job, and ingestion into Postgres
is `internal/ingestion`'s. Those are deliberately separate stages.

## Running it

Always start with the two read-only commands. Neither downloads a PDF.

```sh
# 1. What would be crawled? Reads listing pages only.
go run ./cmd/crawler discover -limit 20

# 2. Which link would be downloaded per page? Fetches page HTML and probes
#    candidates with a 1 KB ranged GET. Still downloads no PDFs.
go run ./cmd/crawler resolve -limit 20

# 3. The real thing.
go run ./cmd/crawler run -data ./data
```

Then, offline:

```sh
python workers/extraction/triage.py ./data/raw
python workers/extraction/extract.py ./data/raw/<doc-id>/original.pdf \
    --language en --out ./data/structured/<doc-id>/
```

### Flags worth knowing

| Flag | Default | Notes |
|---|---|---|
| `-limit` | 0 (none) | Stop after N law pages. **Always set this on a first run.** |
| `-rps` | 0.5 | Requests/second. This is a government site that owes us nothing — leave it alone. |
| `-concurrency` | 2 | Documents in flight. The rate limiter, not this, decides load. |
| `-resume` | true | Skips documents already in a terminal state. A long crawl *will* be interrupted. |
| `-refetch` | false | Re-download even when the PDF is on disk. For recovering a bad run. |
| `-data` | `./data` | Store root. Gitignored. |
| `-v` | false | Debug logging, including per-page discovery counts. |

Ctrl-C stops politely: in-flight requests cancel, every record written so far
stays on disk, and `-resume` picks up from there.

### Git Bash gotcha

MSYS rewrites arguments that look like Unix paths into Windows paths, so
`-listings /en/laws/proclamations/` arrives as `C:/Program Files/Git/en/...`.
Prefix the command with `MSYS_NO_PATHCONV=1`, or omit `-listings` to use all
three defaults.

## Why the PDF resolver is the way it is

Ministry law pages carry a JetElements download button that *looks* like the
download link:

```html
<a class="... jet-download ..." href="https://justice.gov.et/en/?jet_download=cb734b98…">
```

It frequently answers **HTTP 500**, while the real file is linked from a
separate element on the same page:

```html
<a class="jepe-download-button" href="https://justice.gov.et/wp-content/uploads/2026/06/አዋጅ-ቁጥር-1415-2018.pdf">
```

One real page carried **13** `?jet_download=` links alongside one working
`jepe-download-button`. So candidates are ranked, and a failure is a reason to
try the next one rather than to fail the document:

1. `a.jepe-download-button[href]`
2. any href resolving to a direct `.pdf`
3. `iframe` / `embed` / `object` pointing at a PDF
4. `a.jet-download[href]` — discovery only, never trusted

Validation is by **content, never extension**: HTTP 200 plus a `%PDF-` magic
header, with `Content-Type` only able to rule things out. A `.pdf` URL serving
an HTML error page with HTTP 200 is the single most common way a crawl quietly
poisons a corpus.

## Traps this handles (and you should not "simplify" away)

- **The English page links Amharic-named PDFs.** Every one seen so far.
  `page_language` is provenance only — the document's real language must come
  from the extracted text. `ingestion.Service.Ingest` already refuses a
  document/extraction language mismatch, so a wrong guess fails loudly.
- **Two calendars.** The same instrument is `1234/2013` on its Amharic page and
  `1234/2021` on its English one. The calendar follows *the page*, not the
  number — no year threshold works, because Proclamation 179 is `179/1991` E.C.
  and `179/1998` G.C. and both are plausible years. `GregorianYear` refuses to
  convert rather than guess; a date wrong by a year changes which law the
  in-force SQL filters return.
- **Slugs are not filenames.** They are the law's decoded Amharic title — long
  enough to threaten Windows' MAX_PATH, and able to contain `/` or `..`.
  `SafeDocID` sanitises, caps, and appends a digest of the full slug so laws
  sharing a long title prefix cannot collide.
- **Not every page has a `Law ID:` field.** Loan-ratification proclamations
  don't; the number is recovered from the title instead.
- **Chrome viewer URLs.** `chrome-extension://…/https://…pdf` is unwrapped and
  never stored — it is not a location anything can fetch.

## Storage layout

```
data/
  raw/        <doc-id>/original.pdf
  extracted/  <doc-id>/text.txt          ← workers/extraction writes this
  structured/ <doc-id>/document.json     ← ingestion.Extraction shape
  metadata/   <doc-id>.json              ← crawl record: state, provenance, checksum
  index/      checksums.json             ← sha256 → doc-id, for dedupe
```

Dedupe is by **sha256**, and survives restarts. The Ministry links the same
consolidated PDF from more than one law page; ingesting it twice would put every
article under two document identities. Duplicates are kept on disk and marked
`duplicate_of`, not deleted.

## Known open questions

- **Discovery returned 61 law pages across all three listings**, but the
  proclamations listing alone is 7 pages × 20 links. Either the listing pages
  repeat documents, or the walk stops early. **Check this before trusting a full
  crawl.** Run `discover -v` and read the `listing page walked` lines, which
  report `links` vs `new` per page: if `new` hits 0 while pages still return
  200, 61 is right; if the walk stops while pages still yield new links, the
  stop condition in `walkListing` needs fixing.
- **Pagination links are not in the HTML** — the listing is AJAX-driven. The
  path form (`/en/laws/proclamations/page/N/`) does work, which is what
  discovery uses, so the stop condition is doing all the work.
- **Past-the-end pages return HTTP 500, not 404**, so the client spends its full
  retry budget (~30s of backoff) at the end of each listing before concluding it
  is done. Harmless, just slow.
- **Newest-first ordering surfaces the least useful documents.** The first pages
  of the proclamations listing are loan ratifications — 3 pages, ~1 article
  each. `triage.py` flags them "clean text but no article structure found",
  which is the documents, not the parser. Crawl by sector rather than recency
  when going for corpus value.
- **Amharic pages are not crawled by default.** `DefaultIndexPaths` are the
  `/en/` listings. The corpus currently has **zero** Amharic rows, so the
  `/am/law/<slug>/` pages are the higher-value target; the slug is shared, so
  pairing them is trivial.

## Tests

```sh
go test ./internal/crawler/          # fixtures and httptest only, no network
go test -race ./internal/crawler/
```

`testdata/federal-courts-proclamation.html` is a verbatim capture of a live law
page, kept whole rather than trimmed — its job is to fail when the site is
redesigned, and a fixture edited down to what currently matters cannot do that.
