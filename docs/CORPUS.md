# Corpus assessment

**Source:** `C:\Users\PC\Documents\ethiopian law` — 79 PDFs, 5 Word documents
**Assessed:** 2026-08-15, with `workers/extraction/triage.py`
**Re-run:** `python triage.py "<folder>" --json triage.json`

## Verdict

The corpus is in better shape than expected, but not where you would expect. The
Amharic documents are mostly unusable and the *English* editions of the major codes are
clean — which inverts the plan's assumption that Amharic would carry the corpus.

| Verdict | Files | Pages | What it means |
|---|---:|---:|---|
| `native` | 23 | 4271 | Real text layer. Extract directly. |
| `junk` | 12 | 1557 | Has a full text layer, but it is not language. Needs OCR. |
| `scanned` | 24 | 1909 | No text layer at all. Needs OCR. |
| `legacy` | 2 | 157 | Ethiopic in a pre-Unicode font encoding. Needs a conversion table. |
| `damaged` | 1 | 0 | Will not open. |

17 further files are byte-identical duplicates — `Main ones/` and `constitutions/` hold
the same documents under two names. Ingesting both would double every retrieval result
for those laws.

## The trap: a text layer that is not text

Twelve documents carry a *complete* text layer that is pure OCR debris. `Commercial Code
(Amharic).pdf` is 351 pages, every page yields ~2000 characters, and the characters are:

```
17 | f''''~ | . at.., | !~ | Ft. | . fDIJ.lP~ | . (j)~" | . "7:}:}"If.
```

Someone OCR'd Amharic scans with an English-only engine years ago. Page count, file size
and character count all look healthy; nothing but reading the text reveals it.

**This is the most dangerous category in the corpus.** Ingesting it does not fail — it
produces chunks that embed to noise and then get returned as sources for real legal
questions. So both `triage.py` and `extract.py` now score every document on whether its
text looks like language at all (`text_quality`: the share of tokens that are three or
more Latin letters or contain two adjacent Ethiopic characters), and the extractor
**refuses** anything below 0.60 rather than warning about it. The distribution is cleanly
bimodal — real documents score 0.62–0.99, debris scores 0.07–0.48 — so the threshold is
not a delicate judgement call.

A second, separate failure mode: `NBE-etablisheme-591.pdf` and the Tigray constitution
carry real Amharic set in a **pre-Unicode font**, where syllables are mapped onto Latin-1
codepoints (`¾wN?^© v”¡”` is `የብሔራዊ ባንክ`). Font names cannot be trusted — one of these
embeds its legacy font renamed to "Times-Roman" — so it is detected by encoding instead:
a dense run of characters that appear in no real Amharic or English text. These are
recoverable with a per-family conversion table, which is not built.

## Extracted today

22 documents, 4251 pages, **8207 articles**, no OCR required:

| Articles | Pages | Lang | Document |
|---:|---:|:--:|---|
| 1970 | 586 | en | Civil Code |
| 891 | 270 | en | Commercial Code (1960) |
| 823 | 611 | en | Commercial Code No. 1243/2021 |
| 821 | 358 | am | አዲሱ የንግድ ሕግ (Commercial Code 1243, Amharic) |
| 661 | 530 | en | Criminal Code |
| 448 | 232 | en | Civil Procedure Code |
| 248 | 96 | en | Family Code |
| 198 | 97 | en | Criminal Procedure Code |
| 131 | 29 | en | 1955 Revised Constitution |
| 55 | 6 | en | 1931 Constitution |
| 24 | 10 | en | SNNP Regional Constitution |
| 18 | 28 | en | 1987 Constitution |

Every one of these has a complete section path — `Book 2 — BUSINESS ORGANIZATIONS ›
Title 6 — SHARE COMPANY › Chapter 4 — MANAGEMENT OF THE COMPANY` — which is what the
context header prepended to each chunk is built from.

The two editions of Commercial Code 1243 recover 823 (English) and 821 (Amharic) of the
document's 825 articles. Because the two editions disagree about *which* are missing, the
handful of misses are extraction failures rather than gaps in the law; the markers are
absent from those pages' text layer entirely.

### Not statutes

Six of the 22 fell back to window chunking because they have no article structure to
find. Three are worth separating out by hand rather than by pattern:

- **`United States Sentencing Commission Guideline Manual.pdf`** (636 pages, 804 windows)
  is United States law. It must not enter an Ethiopian legal corpus — a question about
  Ethiopian sentencing would retrieve American guidelines and cite them as authority.
- `Magisterate Court Sentencing Guidelines.pdf` and `የአማራ_ክልል_የቤተሰብ_ሕግ_መሠረታዊ_ፅንሰ_ሐሳቦች.pdf`
  read as guidance and teaching material — useful context, but they are not provisions
  and should not be cited as if they were.

## What is needed, in order of value

1. **Better copies of the Amharic codes.** The Civil, Commercial, Criminal, and Civil
   Procedure codes all exist here in Amharic and all are unusable. This is the single
   highest-value ask for the Addis Ababa professors: a clean Amharic text of any one of
   these is worth more than all the OCR work below.
2. **`Civil Code (Amharic).pdf` is corrupt** — MuPDF cannot open it (`zlib error: invalid
   distance too far back`). Needs re-downloading regardless.
3. **OCR for 3466 pages** across 36 documents. Tesseract with `amh` data is not installed
   on this machine, so nothing here has been OCR'd yet, and phone-photographed pages will
   need deskew and contrast work before Tesseract does anything useful with Ethiopic.
   Worth attempting on a handful of pages to measure accuracy before committing to it.
4. **A legacy-encoding converter** for the pre-Unicode Ethiopic documents. Bounded work
   with a known shape (a per-family character table), and unlike OCR it is lossless.
5. **The 5 Word documents** are not handled by the extractor at all.

## Recommended first corpus

The plan's milestone asks for 10–20 documents and 50 questions. The eight major English
codes plus the Amharic Commercial Code make a coherent, high-quality starting set of
**nine documents and roughly 6200 articles**, weighted toward commercial and civil law —
which is what the PRD's flagship example question is about.

Note what this means for the evaluation gate: with only one usable Amharic document, the
language-split Precision@5 target in the plan cannot be measured meaningfully yet. Either
the eval set is English-first until better Amharic sources arrive, or the Amharic half
rests entirely on Commercial Code 1243.

## How documents now reach the database

Since the corpus bridge (`cmd/ingest`) was built, the three stages are
connected and each document's outcome is recorded:

```
cmd/crawler run      → data/raw/<id>/original.pdf, data/metadata/<id>.json
extract.py --store-root → data/structured/<id>/document.json
cmd/ingest           → Postgres rows + vectors, data/ingest/<id>.json
```

`cmd/ingest` takes each document's title, type, number and publication date from
the crawl record instead of asking an operator to retype them, and decides
whether to publish from the extraction's own numbers. The gates exist because of
what this assessment found:

- **Pages with no text layer.** OCR is still not wired up, so the 24 `scanned`
  and 12 `junk` documents here are exactly what the pipeline will keep meeting.
  A 300-page scan carrying five incidental text pages passes the 0.60
  text-quality refusal — those five pages are real text — and would otherwise be
  ingested as a five-article "code". This is the gate that catches it.
- **Window fallback.** The six documents with no article structure are ingested
  as drafts rather than published, so they are inspectable without being
  citable.
- **Script against declared language.** Nothing previously checked the
  `--language` an operator typed against what the PDF holds, and an Amharic
  document stored as English is indexed with the English stemmer and given an
  English context header. Silent, and permanent.
- **Repealed and amended laws.** Ministry law pages state this, and it was being
  discarded. A repealed proclamation's page carries no repeal date, so the
  in-force filter would have read it as current law.
- **Documents that parse perfectly and do not belong.** No mechanical gate
  catches the US Sentencing Commission manual noted above. `cmd/ingest -deny`
  takes a file of document ids, which is where that judgement is recorded.

Byte-identical duplicates are handled in two places now: the crawler marks them
`duplicate_of` and the ingest driver skips them, and `document_versions`
additionally carries the file's sha256 under a corpus-wide unique index — so the
same PDF cannot enter twice even if its law page is retitled between crawls and
the title lookup misses.
