#!/usr/bin/env python3
"""Assess a folder of legal PDFs before ingesting any of them.

    python triage.py "C:/Users/PC/Documents/ethiopian law" --json triage.json

Answers the only question that matters before building a corpus: for each
document, is there a text layer worth extracting, or is it photographs of paper?
The two need completely different pipelines, and the difference is invisible from
a file listing — a 40 MB PDF can be 400 pages of clean text or 30 phone snaps.

The trap this tool exists to catch: a PDF can have a *full* text layer that is
complete garbage. Several documents here were OCR'd years ago with an
English-only engine over Amharic scans, and their text layer reads

    17 | f''''~ | . at.., | !~ | Ft. | . fDIJ.lP~

Page count, character count and file size all look healthy. Ingesting that is
worse than ingesting nothing: the chunks embed to noise, and retrieval will
return them for real questions. So every document is also scored on whether its
text looks like language at all.

Per document it reports pages, how much text the layer yields, that text's
quality, how much of it is Ethiopic, how many article markers the extractor's own
patterns find, and a verdict:

    native   — real text layer, extract directly
    junk     — has a text layer, but it is not language; OCR from the images
    legacy   — Ethiopic in a pre-Unicode font encoding; needs a conversion table
    partial  — some pages have text, some are images; OCR the rest
    scanned  — no text layer, OCR everything
    damaged  — the file could not be opened

Nothing is modified. This only reads.
"""

from __future__ import annotations

import argparse
import dataclasses
import hashlib
import json
import re
import sys
import unicodedata
from collections import defaultdict
from pathlib import Path

# Chars on a page below which the text layer is treated as absent. Matches
# extract.py's OCR threshold, so the two agree about what "no text" means.
EMPTY_PAGE_CHARS = 40

# Share of pages that must carry text for a document to count as native.
NATIVE_PAGE_RATIO = 0.85

# Below this, a document with some text is still mostly images.
PARTIAL_PAGE_RATIO = 0.10

# Share of whitespace-separated tokens that must look like words.
#
# Calibrated against this corpus, where the distribution is bimodal with a wide
# gap: clean documents score 0.62–0.99, and everything unusable scores 0.48 or
# below. Both failure modes — bad OCR and legacy font encoding — sit in the lower
# cluster, and nothing real sits between them.
MIN_TEXT_QUALITY = 0.60

# Share of characters that are neither ASCII nor Ethiopic. Legacy Ethiopic fonts
# map syllables onto Latin-1 and General Punctuation codepoints, so their text
# extracts as a dense hail of ¾ ” ‹ Ÿ ° — real prose in any language has almost
# none of these.
MAX_FOREIGN_GLYPH_RATIO = 0.12

# A run of Latin letters long enough to be a word rather than debris.
LATIN_WORD = re.compile(r"^[A-Za-z]{3,}$")

# Two adjacent Ethiopic characters — Amharic words are never one syllable of
# noise surrounded by punctuation.
ETHIOPIC_RUN = re.compile(r"[ሀ-፿]{2,}")

STRIP_CHARS = ".,;:!?()[]{}\"'`~*_-–—|/\\፡።፣፤፥፦፧፨"

# Pre-Unicode Ethiopic font families. Text set in these is real Amharic or
# Tigrinya, but the bytes are Latin codepoints that only render correctly with
# the font installed — so it extracts as gibberish and needs a per-family
# conversion table, not OCR.
LEGACY_ETHIOPIC_FONTS = (
    "vg2", "vg2000", "visualgeez", "powergeez", "geezbasic", "agafari",
    "ethiopic-wash", "brana",
)

# The same families' Unicode-native descendants, plus the standard system fonts.
UNICODE_ETHIOPIC_FONTS = (
    "visualgeezunicode", "nyala", "abyssinica", "ebrima", "notosansethiopic",
    "washrab", "jiret",
)


@dataclasses.dataclass
class Assessment:
    path: str
    verdict: str
    pages: int = 0
    chars: int = 0
    chars_per_page: int = 0
    text_pages: int = 0
    empty_pages: int = 0
    ethiopic_ratio: float = 0.0
    foreign_glyph_ratio: float = 0.0
    images: int = 0
    article_markers_en: int = 0
    article_markers_am: int = 0
    language_guess: str = ""
    text_quality: float = 0.0
    legacy_fonts: list[str] = dataclasses.field(default_factory=list)
    sha256: str = ""
    duplicate_of: str = ""
    note: str = ""

    @property
    def ocr_pages(self) -> int:
        """Pages that must be rendered and OCR'd to become usable."""
        if self.verdict in ("junk", "scanned"):
            return self.pages
        return self.empty_pages

    @property
    def markers(self) -> int:
        return max(self.article_markers_en, self.article_markers_am)


def ethiopic_ratio(text: str) -> float:
    """Share of letters that are Ethiopic — how the language guess is made."""
    ethiopic = latin = 0
    for ch in text:
        if "\u1200" <= ch <= "\u137f":
            ethiopic += 1
        elif ch.isalpha() and ch.isascii():
            latin += 1
    total = ethiopic + latin
    return ethiopic / total if total else 0.0


def text_quality(text: str) -> float:
    """Share of tokens that look like words in some language.

    This is the gibberish detector, and it is deliberately crude: no dictionary,
    no language model, nothing to maintain. A token counts if it is three or more
    Latin letters, or contains two adjacent Ethiopic characters. Bad OCR produces
    neither — it produces single characters wearing punctuation.
    """
    tokens = text.split()
    if not tokens:
        return 0.0

    good = countable = 0
    for token in tokens:
        stripped = token.strip(STRIP_CHARS)
        if not stripped or stripped.isdigit():
            # Numbers are legitimate and abundant in legal text, but a page of
            # page numbers is not evidence of a good text layer either, so they
            # are neither counted for nor against.
            continue
        countable += 1
        if LATIN_WORD.match(stripped) or ETHIOPIC_RUN.search(stripped):
            good += 1

    return good / countable if countable else 0.0


def foreign_glyph_ratio(text: str) -> float:
    """Share of visible characters that are neither ASCII nor Ethiopic.

    This is the legacy-font detector, and it is needed because font names cannot
    be trusted: several documents here embed a pre-Unicode Ethiopic font renamed
    to "Times-Roman". The encoding gives it away regardless — legacy families map
    syllables onto Latin-1 and General Punctuation, so the extracted text is full
    of characters that appear in no real Amharic or English document.
    """
    foreign = visible = 0
    for ch in text:
        if ch.isspace():
            continue
        visible += 1
        if ch.isascii() or 0x1200 <= ord(ch) <= 0x137F:  # ASCII or Ethiopic
            continue
        # Latin-1 Supplement through Spacing Modifiers, and General
        # Punctuation: where legacy Ethiopic encodings put their syllables.
        code = ord(ch)
        if 0x00A0 <= code <= 0x02FF or 0x2000 <= code <= 0x206F:
            foreign += 1
    return foreign / visible if visible else 0.0


def classify_fonts(names: set[str]) -> tuple[list[str], list[str]]:
    """Split font names into legacy-Ethiopic and Unicode-Ethiopic families."""
    legacy: list[str] = []
    unicode_eth: list[str] = []

    for name in sorted(names):
        # PDF font names carry a subset tag, e.g. "GBCGFA+VG2Main".
        bare = name.split("+")[-1].replace(" ", "").replace("-", "").lower()
        if any(marker in bare for marker in UNICODE_ETHIOPIC_FONTS):
            unicode_eth.append(name)
        elif any(marker in bare for marker in LEGACY_ETHIOPIC_FONTS):
            legacy.append(name)

    return legacy, unicode_eth


def file_sha256(path: Path) -> str:
    """Hash a file so byte-identical copies in different folders are detectable.

    This corpus keeps the same constitutions under two directory names; ingesting
    both would double every retrieval result for those documents.
    """
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        for block in iter(lambda: fh.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def assess(path: Path) -> Assessment:
    try:
        import pymupdf
    except ImportError as exc:  # pragma: no cover
        raise SystemExit(f"PyMuPDF is not installed: pip install -r requirements.txt ({exc})") from exc

    # Imported here so the module stays importable without the extractor's deps.
    from extract import ARTICLE_PATTERNS

    try:
        doc = pymupdf.open(path)
    except Exception as exc:  # noqa: BLE001 - a corrupt PDF can raise anything
        return Assessment(path=str(path), verdict="damaged", note=str(exc)[:120])

    result = Assessment(path=str(path), verdict="")
    chunks: list[str] = []
    fonts: set[str] = set()

    try:
        result.pages = doc.page_count
        for page in doc:
            text = page.get_text("text") or ""
            chunks.append(text)

            if len(text.strip()) >= EMPTY_PAGE_CHARS:
                result.text_pages += 1
            else:
                result.empty_pages += 1

            try:
                result.images += len(page.get_images(full=False))
                fonts.update(font[3] for font in page.get_fonts(full=False))
            except Exception:  # noqa: BLE001 - resource listing is best effort
                pass
    finally:
        doc.close()

    text = unicodedata.normalize("NFC", "\n".join(chunks))
    result.chars = len(text.strip())
    result.chars_per_page = result.chars // result.pages if result.pages else 0
    result.ethiopic_ratio = round(ethiopic_ratio(text), 3)
    result.text_quality = round(text_quality(text), 3)
    result.foreign_glyph_ratio = round(foreign_glyph_ratio(text), 3)
    result.language_guess = "am" if result.ethiopic_ratio > 0.5 else "en"

    legacy, _ = classify_fonts(fonts)
    result.legacy_fonts = legacy

    result.article_markers_en = len(ARTICLE_PATTERNS["en"].findall(text))
    result.article_markers_am = len(ARTICLE_PATTERNS["am"].findall(text))

    result.verdict, result.note = verdict_for(result)
    return result


def verdict_for(a: Assessment) -> tuple[str, str]:
    """Decide what pipeline a document needs.

    Order matters: the quality check runs before the page-coverage check, because
    a document with a *complete* garbage text layer looks perfect by coverage and
    is the case most likely to be ingested by accident.
    """
    if a.pages == 0:
        return "damaged", "no pages"

    coverage = a.text_pages / a.pages

    if coverage < PARTIAL_PAGE_RATIO:
        return "scanned", "no usable text layer"

    if a.text_quality < MIN_TEXT_QUALITY:
        # Two different failure modes, two different fixes. Legacy encoding is
        # recoverable with a conversion table and keeps the original typesetting;
        # bad OCR can only be redone from the page images.
        if a.legacy_fonts:
            return "legacy", f"pre-Unicode Ethiopic font: {', '.join(a.legacy_fonts[:2])}"
        if a.foreign_glyph_ratio > MAX_FOREIGN_GLYPH_RATIO:
            return "legacy", (f"looks like a legacy Ethiopic encoding "
                              f"({a.foreign_glyph_ratio:.0%} non-ASCII, non-Ethiopic glyphs)")
        return "junk", f"text layer is not language (quality {a.text_quality:.2f})"

    if coverage < NATIVE_PAGE_RATIO:
        return "partial", f"{a.empty_pages} of {a.pages} pages have no text"

    return "native", ""


VERDICT_ORDER = {"native": 0, "partial": 1, "legacy": 2, "junk": 3, "scanned": 4, "damaged": 5}


def print_table(assessments: list[Assessment], stream) -> None:
    rows = sorted(assessments, key=lambda a: (VERDICT_ORDER.get(a.verdict, 9), -a.markers, a.path))

    header = (f"{'verdict':<8} {'pages':>5} {'txt/pg':>8} {'qual':>5} {'eth':>5} "
              f"{'lgcy':>5} {'art':>5} {'dup':>4}  file")
    print(header, file=stream)
    print("-" * len(header), file=stream)

    for a in rows:
        pages = f"{a.text_pages}/{a.pages}"
        dup = "dup" if a.duplicate_of else ""
        print(
            f"{a.verdict:<8} {a.pages:>5} {pages:>8} {a.text_quality:>5.2f} "
            f"{a.ethiopic_ratio:>5.2f} {a.foreign_glyph_ratio:>5.2f} "
            f"{a.markers:>5} {dup:>4}  {Path(a.path).name}",
            file=stream,
        )


def print_summary(assessments: list[Assessment], stream) -> None:
    unique = [a for a in assessments if not a.duplicate_of]

    by_verdict: dict[str, list[Assessment]] = defaultdict(list)
    for a in unique:
        by_verdict[a.verdict].append(a)

    print("\nsummary (duplicates excluded)", file=stream)
    print("-----------------------------", file=stream)
    for verdict in ("native", "partial", "legacy", "junk", "scanned", "damaged"):
        group = by_verdict.get(verdict, [])
        if not group:
            continue
        pages = sum(a.pages for a in group)
        ocr = sum(a.ocr_pages for a in group)
        print(f"  {verdict:<8} {len(group):>3} files {pages:>6} pages {ocr:>6} pages needing OCR",
              file=stream)

    duplicates = [a for a in assessments if a.duplicate_of]
    if duplicates:
        print(f"\n  {len(duplicates)} byte-identical duplicates (ignore these copies):", file=stream)
        for a in sorted(duplicates, key=lambda a: a.path):
            print(f"    {Path(a.path).name}\n      copy of {a.duplicate_of}", file=stream)

    print(f"\n  total pages needing OCR: {sum(a.ocr_pages for a in unique)}", file=stream)

    ready = sorted(
        (a for a in by_verdict.get("native", []) if a.markers >= 3),
        key=lambda a: -a.markers,
    )
    print(f"\n  ingestible today, no OCR: {len(ready)} files", file=stream)
    for a in ready:
        print(f"    {a.markers:>5} articles  {a.language_guess}  {a.pages:>4}p  {Path(a.path).name}",
              file=stream)

    thin = [a for a in by_verdict.get("native", []) if a.markers < 3]
    if thin:
        print(f"\n  clean text but no article structure found: {len(thin)} files", file=stream)
        print("    (the extractor would fall back to window chunking — check the heading form)",
              file=stream)
        for a in sorted(thin, key=lambda a: -a.pages):
            print(f"    {a.pages:>5}p  {a.language_guess}  {Path(a.path).name}", file=stream)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("root", type=Path, help="folder to scan recursively")
    parser.add_argument("--json", type=Path, default=None, help="write the full report here")
    args = parser.parse_args(argv)

    # Half this corpus has Amharic filenames, and the default Windows console
    # encoding cannot represent them — printing the report would die on the
    # first one.
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")
        except (AttributeError, OSError):
            pass

    if not args.root.is_dir():
        print(f"error: {args.root} is not a directory", file=sys.stderr)
        return 1

    pdfs = sorted(p for p in args.root.rglob("*") if p.suffix.lower() == ".pdf")
    others = sorted(p for p in args.root.rglob("*") if p.suffix.lower() in {".doc", ".docx"})

    if not pdfs:
        print(f"error: no PDFs under {args.root}", file=sys.stderr)
        return 1

    assessments: list[Assessment] = []
    by_hash: dict[str, str] = {}

    for i, path in enumerate(pdfs, 1):
        print(f"[{i}/{len(pdfs)}] {path.name}", file=sys.stderr)

        result = assess(path)
        result.sha256 = file_sha256(path)
        if original := by_hash.get(result.sha256):
            result.duplicate_of = original
        else:
            by_hash[result.sha256] = str(path)
        assessments.append(result)

    print_table(assessments, sys.stdout)
    print_summary(assessments, sys.stdout)

    if others:
        print(f"\n  {len(others)} Word documents found — not handled by this extractor:", file=sys.stdout)
        for p in others:
            print(f"    {p.name}", file=sys.stdout)

    if args.json:
        args.json.write_text(
            json.dumps([dataclasses.asdict(a) for a in assessments], ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        print(f"\nreport written to {args.json}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
