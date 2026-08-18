#!/usr/bin/env python3
"""Turn an Ethiopian legal PDF into the structured JSON the Go ingester reads.

Run it by hand, read the report it prints, then ingest. It is not a service —
see README.md for why, and for the output contract.

    python extract.py commercial_code.pdf --language en --out out/
    python extract.py labor.pdf --language am --out out/ --ocr
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import re
import sys
import unicodedata
from pathlib import Path

from geez import AMHARIC_NUMBER_WORDS, normalize_number

# PyMuPDF is imported lazily, inside extract_pages. Structure detection,
# numeral normalisation and windowing are pure text functions, and keeping them
# importable without the PDF stack is what makes them testable.

# ---------------------------------------------------------------------------
# Structure detection
#
# Legal drafters are consistent within a document and inconsistent between them.
# These patterns cover the forms seen in Ethiopian proclamations, regulations
# and codes; a document that matches none of them falls back to window chunking
# and is reported loudly rather than silently ingested as one giant article.
# ---------------------------------------------------------------------------

ARTICLE_PATTERNS = {
    "en": re.compile(
        r"^[ \t]*(?:ARTICLE|Article|Art\.)[ \t]*(\d+[A-Za-z]?)[ \t]*[.\-–—:)]?[ \t]*(.*)$",
        re.MULTILINE,
    ),
    "am": re.compile(
        r"^[ \t]*አንቀጽ[ \t]*([፩-፼]+|\d+[A-Za-z]?)[ \t]*[.\-–—:)]?[ \t]*(.*)$",
        re.MULTILINE,
    ),
}

# The same markers, split across two lines.
#
# PDF text extraction follows the page's layout, not its logic, so a marker that
# straddles a line or column break arrives as "አንቀጽ\n424". In the Amharic
# commercial code this accounts for 42 of 825 articles — five percent of the law,
# silently missing, with nothing downstream able to notice the absence.
#
# Both sides are anchored hard: the keyword must be alone on its line and the
# number must be alone on the next. Without that, every cross-reference in the
# body ("...በዚህ ሕግ አንቀጽ\n425 እስከ 441...") would start a phantom article.
ARTICLE_PATTERNS_SPLIT = {
    "en": re.compile(
        r"^[ \t]*(?:ARTICLE|Article|Art\.)[ \t]*\n[ \t]*(\d+[A-Za-z]?)[ \t]*[.\-–—:)]?[ \t]*$",
        re.MULTILINE,
    ),
    "am": re.compile(
        r"^[ \t]*አንቀጽ[ \t]*\n[ \t]*([፩-፼]+|\d+[A-Za-z]?)[ \t]*[.\-–—:)]?[ \t]*$",
        re.MULTILINE,
    ),
}

# English legal drafters number divisions with words too — this corpus's
# commercial code writes "CHAPTER ONE", never "CHAPTER 1" or "CHAPTER I". Roman
# numerals and digits appear in older statutes, so all three are accepted.
ENGLISH_NUMBER_WORDS = {
    "ONE": 1, "TWO": 2, "THREE": 3, "FOUR": 4, "FIVE": 5, "SIX": 6,
    "SEVEN": 7, "EIGHT": 8, "NINE": 9, "TEN": 10, "ELEVEN": 11, "TWELVE": 12,
    "THIRTEEN": 13, "FOURTEEN": 14, "FIFTEEN": 15, "SIXTEEN": 16,
    "SEVENTEEN": 17, "EIGHTEEN": 18, "NINETEEN": 19, "TWENTY": 20,
    "FIRST": 1, "SECOND": 2, "THIRD": 3, "FOURTH": 4, "FIFTH": 5, "SIXTH": 6,
    "SEVENTH": 7, "EIGHTH": 8, "NINTH": 9, "TENTH": 10,
}

ROMAN_NUMERAL = re.compile(r"^[IVXLC]+$")
ROMAN_VALUES = {"I": 1, "V": 5, "X": 10, "L": 50, "C": 100}


def roman_to_int(text: str) -> int | None:
    """Convert a Roman numeral, or None if it is malformed."""
    total = 0
    previous = 0
    for ch in reversed(text):
        value = ROMAN_VALUES.get(ch)
        if value is None:
            return None
        total += -value if value < previous else value
        previous = max(previous, value)
    return total or None


def normalize_division_number(raw: str) -> str:
    """Normalise a book/part/title/chapter number to Arabic numerals."""
    upper = raw.strip().upper()
    if upper in ENGLISH_NUMBER_WORDS:
        return str(ENGLISH_NUMBER_WORDS[upper])
    if ROMAN_NUMERAL.match(upper):
        if value := roman_to_int(upper):
            return str(value)
    return normalize_number(raw)


def _english_heading_patterns() -> list[tuple[str, re.Pattern[str]]]:
    """Heading patterns for English.

    Two constraints do the real work here, and both were learned from a wrong
    result rather than guessed.

    The keyword must be UPPERCASE. Real headings are set in capitals; the
    lower-case forms are prose cross-references ("Book II of this Code applicable
    to business organizations."), and matching those replaced 750 articles' worth
    of section path with a sentence fragment.

    The number must be a complete token, not followed by a letter. Without that,
    "Book is to protect the legitimate interests of creditors." parses as Book I
    with the title "s to protect the legitimate interests of creditors" — the "i"
    of "is" read as a Roman numeral.
    """
    words = "|".join(sorted(ENGLISH_NUMBER_WORDS, key=len, reverse=True))
    number = rf"(?:{words}|[IVXLC]+|\d+)"

    patterns: list[tuple[str, re.Pattern[str]]] = []
    for kind, keyword in [("book", "BOOK"), ("part", "PART"), ("title", "TITLE"),
                          ("chapter", "CHAPTER"), ("section", "SECTION")]:
        patterns.append((kind, re.compile(
            rf"^[ 	]*{keyword}[ 	]+({number})(?![A-Za-z])[ 	]*[.\-–—:]?[ 	]*(.*)$",
            re.MULTILINE)))
    return patterns


def _amharic_heading_patterns() -> list[tuple[str, re.Pattern[str]]]:
    """Heading patterns for Amharic, where divisions are numbered in words.

    "ምዕራፍ አንድ" is Chapter One; "ምዕራፍ ፩" does not occur. The number also moves:
    a book is written both "መጽሐፍ አንድ" and "አንደኛ መጽሐፍ", so each keyword needs a
    pattern in each order.

    Restricting the number to the known word list is what keeps this safe. A
    permissive "ምዕራፍ <anything>" would match the very common sentence opener
    "ምዕራፍ ድንጋጌዎች" and shred the section path.
    """
    words = "|".join(sorted(AMHARIC_NUMBER_WORDS, key=len, reverse=True))
    number = rf"(?:[፩-፼]+|\d+|{words})"

    keywords = [
        ("book", "መጽሐፍ"),
        ("part", "ክፍል"),
        ("title", "ርዕስ"),
        ("chapter", "ምዕራፍ"),
    ]

    patterns: list[tuple[str, re.Pattern[str]]] = []
    for kind, keyword in keywords:
        patterns.append((kind, re.compile(
            rf"^[ \t]*{keyword}[ \t]+({number})[ \t]*[.\-–—:]?[ \t]*(.*)$", re.MULTILINE)))
        # Reversed order: "አንደኛ መጽሐፍ".
        patterns.append((kind, re.compile(
            rf"^[ \t]*({words})[ \t]+{keyword}[ \t]*[.\-–—:]?[ \t]*(.*)$", re.MULTILINE)))
    return patterns


# Ordered outermost-first: a match replaces everything at its level and below,
# which is how the section path stays consistent across a document.
HEADING_PATTERNS = {
    "en": _english_heading_patterns(),
    "am": _amharic_heading_patterns(),
}

HEADING_LEVELS = {"book": 0, "part": 1, "title": 2, "chapter": 3, "section": 4}

# A line under a heading longer than this is body text, not the heading's title.
HEADING_TITLE_MAX_CHARS = 80

# A page whose text layer yields less than this is treated as scanned.
OCR_CHAR_THRESHOLD = 40

# Fallback chunking, used only when structure detection fails.
WINDOW_CHARS = 2500
WINDOW_OVERLAP = 375  # 15%

# Below this many articles, a document is assumed to have parsed badly.
MIN_ARTICLES = 3

# Minimum share of tokens that must look like words for a text layer to be
# trusted. See triage.py, which calibrated this against the corpus: a PDF can
# carry a complete text layer that is pure OCR debris, and ingesting it is worse
# than ingesting nothing — the chunks embed to noise and retrieval returns them
# for real questions.
MIN_TEXT_QUALITY = 0.60

LATIN_WORD = re.compile(r"^[A-Za-z]{3,}$")
ETHIOPIC_RUN = re.compile(r"[ሀ-፿]{2,}")
QUALITY_STRIP_CHARS = ".,;:!?()[]{}\"'`~*_-–—|/\\፡።፣፤፥፦፧፨"


class LowQualityText(Exception):
    """Raised when a document's text layer is not language.

    Carries the numbers so the operator can see how far off it was, and knows to
    reach for OCR rather than for a different heading pattern.
    """

    def __init__(self, path: Path, quality: float) -> None:
        self.path = path
        self.quality = quality
        super().__init__(
            f"{path.name}: text layer quality {quality:.2f} is below {MIN_TEXT_QUALITY:.2f} — "
            f"it is not language. This document needs OCR from its page images "
            f"(see triage.py). Pass --allow-low-quality to override."
        )


@dataclasses.dataclass
class Article:
    article_no: str
    article_no_raw: str
    title: str
    chapter: str
    section_path: list[str]
    text: str
    page_no: int
    char_start: int
    char_end: int
    structure_confidence: str


@dataclasses.dataclass
class Report:
    source_file: str
    language: str
    pages: int
    pages_empty: int
    pages_ocred: int
    articles: int
    fallback: bool
    warnings: list[str]
    number_range: str = ""
    gaps: list[str] = dataclasses.field(default_factory=list)

    def print_to(self, stream) -> None:
        print(f"\n{self.source_file}", file=stream)
        print(f"  language          : {self.language}", file=stream)
        print(f"  pages             : {self.pages}", file=stream)
        print(f"  pages with no text: {self.pages_empty}", file=stream)
        print(f"  pages OCR'd       : {self.pages_ocred}", file=stream)
        print(f"  articles detected : {self.articles}", file=stream)
        if self.number_range:
            print(f"  numbering         : {self.number_range}", file=stream)
        if self.gaps:
            shown = ", ".join(self.gaps[:15])
            more = f" (+{len(self.gaps) - 15} more)" if len(self.gaps) > 15 else ""
            print(f"  MISSING numbers   : {shown}{more}", file=stream)
        print(f"  structure         : {'WINDOW FALLBACK' if self.fallback else 'article-level'}",
              file=stream)
        for w in self.warnings:
            print(f"  ! {w}", file=stream)


# ---------------------------------------------------------------------------
# Text extraction
# ---------------------------------------------------------------------------


def extract_pages(pdf_path: Path, use_ocr: bool, ocr_lang: str) -> tuple[list[str], int, int]:
    """Return per-page text, plus counts of empty and OCR'd pages."""
    try:
        import pymupdf
    except ImportError as exc:  # pragma: no cover - environment dependent
        raise SystemExit(
            f"PyMuPDF is not installed: pip install -r requirements.txt ({exc})"
        ) from exc

    pages: list[str] = []
    empty = 0
    ocred = 0

    with pymupdf.open(pdf_path) as doc:
        for page in doc:
            text = page.get_text("text") or ""

            if len(text.strip()) < OCR_CHAR_THRESHOLD:
                empty += 1
                if use_ocr:
                    text = ocr_page(page, ocr_lang)
                    if text.strip():
                        ocred += 1

            pages.append(normalize_text(text))

    return pages, empty, ocred


def ocr_page(page, ocr_lang: str) -> str:
    """OCR a single page. Imported lazily so the common path needs no OCR stack."""
    try:
        import io

        import pytesseract
        from PIL import Image
    except ImportError as exc:  # pragma: no cover - environment dependent
        raise SystemExit(
            "OCR requested but its dependencies are missing. "
            "pip install -r requirements.txt, and install the Tesseract binary "
            f"with the 'eng' and 'amh' language data. ({exc})"
        ) from exc

    # 300 DPI: below this, Ethiopic script OCRs poorly.
    pixmap = page.get_pixmap(dpi=300)
    image = Image.open(io.BytesIO(pixmap.tobytes("png")))
    return pytesseract.image_to_string(image, lang=ocr_lang)


def text_quality(text: str) -> float:
    """Share of whitespace-separated tokens that look like words.

    Deliberately crude — no dictionary, no model, nothing to maintain. A token
    counts if it is three or more Latin letters or contains two adjacent Ethiopic
    characters. Bad OCR produces neither; it produces single characters wearing
    punctuation. Digits are excluded from the denominator, since legal text is
    full of legitimate numbers.

    triage.py implements the same measure for whole folders.
    """
    tokens = text.split()
    if not tokens:
        return 0.0

    good = countable = 0
    for token in tokens:
        stripped = token.strip(QUALITY_STRIP_CHARS)
        if not stripped or stripped.isdigit():
            continue
        countable += 1
        if LATIN_WORD.match(stripped) or ETHIOPIC_RUN.search(stripped):
            good += 1

    return good / countable if countable else 0.0


def normalize_text(text: str) -> str:
    """Normalise whitespace and Unicode form.

    NFC matters for Amharic: the same Ethiopic syllable can arrive in more than
    one encoding, and two spellings of one word will not match each other in the
    full-text index.
    """
    text = unicodedata.normalize("NFC", text)
    text = text.replace("\r\n", "\n").replace("\r", "\n")
    text = re.sub(r"[ \t]+", " ", text)
    text = re.sub(r"\n{3,}", "\n\n", text)
    return text


def join_pages(pages: list[str]) -> tuple[str, list[int]]:
    """Concatenate pages, returning the text and each page's start offset."""
    offsets: list[int] = []
    parts: list[str] = []
    cursor = 0

    for page_text in pages:
        offsets.append(cursor)
        parts.append(page_text)
        cursor += len(page_text) + 1  # +1 for the joining newline

    return "\n".join(parts), offsets


def page_for_offset(offsets: list[int], offset: int) -> int:
    """1-based page number containing a character offset."""
    page = 0
    for i, start in enumerate(offsets):
        if start > offset:
            break
        page = i
    return page + 1


# ---------------------------------------------------------------------------
# Article assembly
# ---------------------------------------------------------------------------


def find_structure(text: str, language: str) -> list[tuple[int, str, str, str]]:
    """Find every heading and article marker, in document order.

    Returns (offset, kind, number, title) where kind is "article" or a heading
    level name.
    """
    markers: list[tuple[int, str, str, str]] = []

    for match in ARTICLE_PATTERNS[language].finditer(text):
        markers.append((match.start(), "article", match.group(1), match.group(2).strip()))

    # Markers whose number wrapped onto the next line. These carry no title —
    # what follows the number is the provision's body, and guessing otherwise
    # would put body text in every citation.
    for match in ARTICLE_PATTERNS_SPLIT[language].finditer(text):
        markers.append((match.start(), "article", match.group(1), ""))

    for kind, pattern in HEADING_PATTERNS[language]:
        for match in pattern.finditer(text):
            title = match.group(2).strip() or heading_title_below(text, match.end())
            markers.append((match.start(), kind, match.group(1), title))

    markers.sort(key=lambda m: m[0])
    return markers


def heading_title_below(text: str, offset: int) -> str:
    """Read a heading's title from the line beneath it.

    Legal drafters usually set structural headings over two lines — "CHAPTER 3"
    then "Companies" — so the title is not on the line the number matched. This
    lookahead is applied to headings only, never to articles: beneath an article
    marker is the provision's body, and a body's first line is not a title.
    """
    for line in text[offset:].split("\n"):
        candidate = line.strip()
        if not candidate:
            continue
        # A long line is prose, and any line that is itself a marker belongs to
        # the next heading rather than to this one.
        if len(candidate) > HEADING_TITLE_MAX_CHARS or looks_like_marker(candidate):
            return ""

        # Two signals that the line is a sentence rather than a heading title.
        # Without these, "BOOK II" followed by a wrapped cross-reference yields
        # the section path "Book II — of this Code applicable to business
        # organizations." — which then rides along in the context header of every
        # chunk under that book.
        if candidate.endswith((".", "፡", "።", ",", ";")):
            return ""
        first_letter = next((ch for ch in candidate if ch.isalpha()), "")
        if first_letter.isascii() and first_letter.islower():
            return ""

        return candidate
    return ""


def looks_like_marker(line: str) -> bool:
    """True when a line opens a heading or an article in either language."""
    for pattern in ARTICLE_PATTERNS.values():
        if pattern.match(line):
            return True
    for patterns in HEADING_PATTERNS.values():
        for _, pattern in patterns:
            if pattern.match(line):
                return True
    return False


def build_articles(text: str, offsets: list[int], language: str) -> list[Article]:
    """Assemble articles from the structure markers.

    An article's text runs from its own marker to the next marker of any kind,
    so a chapter heading between two articles does not get swallowed into the
    first one's body.
    """
    markers = find_structure(text, language)
    articles: list[Article] = []

    # One slot per structural level, so a new chapter *replaces* the previous
    # chapter rather than nesting under it, and clears anything below it. A flat
    # list would grow one entry per heading and give every later article a path
    # listing every chapter the document has ever had.
    levels: list[str | None] = [None] * len(HEADING_LEVELS)
    section_path: list[str] = []

    for i, (offset, kind, number, title) in enumerate(markers):
        end = markers[i + 1][0] if i + 1 < len(markers) else len(text)

        if kind != "article":
            level = HEADING_LEVELS[kind]
            label = f"{kind.title()} {normalize_division_number(number)}"
            if title:
                label = f"{label} — {title}"

            levels[level] = label
            for deeper in range(level + 1, len(levels)):
                levels[deeper] = None
            section_path = [s for s in levels if s]
            continue

        body = text[offset:end].strip()
        if not body:
            continue

        articles.append(
            Article(
                article_no=normalize_number(number),
                article_no_raw=number.strip(),
                title=title,
                chapter=section_path[-1] if section_path else "",
                section_path=list(section_path),
                text=body,
                page_no=page_for_offset(offsets, offset),
                char_start=offset,
                char_end=end,
                structure_confidence="high",
            )
        )

    return dedupe_articles(articles)


def missing_numbers(articles: list[Article]) -> list[int]:
    """Numbers absent from the range the document itself spans.

    A legal code numbers its articles consecutively, so a gap is evidence — either
    the law reserves or repeals that number, or the extractor missed a marker
    form. Both are worth an operator's attention, and neither is visible from an
    article count alone: 783 articles looks fine until you notice the document
    goes up to 825.
    """
    numbered = sorted({int(a.article_no) for a in articles if a.article_no.isdigit()})
    if len(numbered) < 2:
        return []
    present = set(numbered)
    return [n for n in range(numbered[0], numbered[-1] + 1) if n not in present]


def dedupe_articles(articles: list[Article]) -> list[Article]:
    """Keep one occurrence per article number: the one with the most text.

    An article number appears several times in a typical legal PDF — once in the
    table of contents, once as a running page header, once as the provision
    itself. Taking the first occurrence takes the table of contents, which yields
    an article whose "text" is its own title and whose chunk cites a page number
    instead of the law. The longest body is the provision.
    """
    best: dict[str, Article] = {}
    for article in articles:
        current = best.get(article.article_no)
        if current is None or len(article.text) > len(current.text):
            best[article.article_no] = article

    # Restore document order, which the numbers alone do not give: amended codes
    # insert 45a between 45 and 46.
    return sorted(best.values(), key=lambda a: a.char_start)


def build_windows(text: str, offsets: list[int]) -> list[Article]:
    """Fallback when article detection fails: fixed windows with overlap.

    Every window is marked low confidence and numbered ``window-N`` rather than
    given a fake article number, because a citation that claims to point at
    Article 12 when it points at "characters 30,000–32,500" is worse than one
    that admits what it is.
    """
    windows: list[Article] = []
    start = 0
    index = 1

    while start < len(text):
        end = min(start + WINDOW_CHARS, len(text))
        body = text[start:end].strip()
        if body:
            windows.append(
                Article(
                    article_no=f"window-{index}",
                    article_no_raw="",
                    title="",
                    chapter="",
                    section_path=[],
                    text=body,
                    page_no=page_for_offset(offsets, start),
                    char_start=start,
                    char_end=end,
                    structure_confidence="low",
                )
            )
            index += 1
        if end == len(text):
            break
        start = end - WINDOW_OVERLAP

    return windows


# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------


def extract(pdf_path: Path, language: str, use_ocr: bool, ocr_lang: str,
            allow_low_quality: bool = False) -> tuple[dict, Report]:
    pages, empty, ocred = extract_pages(pdf_path, use_ocr, ocr_lang)
    text, offsets = join_pages(pages)

    warnings: list[str] = []
    if empty:
        warnings.append(
            f"{empty} of {len(pages)} pages had no text layer"
            + ("" if use_ocr else " — rerun with --ocr if the document is scanned")
        )

    # The quality gate. A document that fails it is refused outright rather than
    # emitted with a warning, because a warning in a batch run is a warning
    # nobody reads, and the cost of ingesting gibberish is a corpus that answers
    # legal questions with OCR debris.
    quality = text_quality(text)
    if quality < MIN_TEXT_QUALITY and not allow_low_quality:
        raise LowQualityText(pdf_path, quality)

    articles = build_articles(text, offsets, language)
    fallback = len(articles) < MIN_ARTICLES

    if fallback:
        warnings.append(
            f"only {len(articles)} articles detected — falling back to window chunking. "
            "Every chunk from this document is marked low confidence and will retrieve worse. "
            "Check whether the document uses a heading form this extractor does not know."
        )
        articles = build_windows(text, offsets)

    gaps = missing_numbers(articles)
    if gaps:
        warnings.append(
            f"{len(gaps)} article numbers in the document's own range were not found. "
            "Either the law skips them, or the extractor missed a marker form — "
            "check a few against the PDF before publishing."
        )

    payload = {
        "source_file": pdf_path.name,
        "language": language,
        "page_count": len(pages),
        "articles": [dataclasses.asdict(a) for a in articles],
        "warnings": warnings,
        "stats": {
            "characters": len(text),
            "pages_without_text_layer": empty,
            "pages_ocred": ocred,
            "structure": "window-fallback" if fallback else "article",
        },
    }

    numbered = sorted(int(a.article_no) for a in articles if a.article_no.isdigit())
    report = Report(
        number_range=f"{numbered[0]}-{numbered[-1]} ({len(numbered)} found)" if numbered else "",
        gaps=[str(g) for g in gaps],
        source_file=pdf_path.name,
        language=language,
        pages=len(pages),
        pages_empty=empty,
        pages_ocred=ocred,
        articles=len(articles),
        fallback=fallback,
        warnings=warnings,
    )
    return payload, report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("pdfs", nargs="+", type=Path, help="PDF files to extract")
    parser.add_argument("--language", required=True, choices=["en", "am"],
                        help="language of the document text")
    parser.add_argument("--out", type=Path, default=Path("out"),
                        help="output directory for the JSON (default: out/)")
    parser.add_argument("--ocr", action="store_true",
                        help="OCR pages that have no text layer")
    parser.add_argument("--ocr-lang", default=None,
                        help="Tesseract language codes (default: amh for --language am, else eng)")
    parser.add_argument("--allow-low-quality", action="store_true",
                        help="extract even if the text layer does not look like language")
    args = parser.parse_args(argv)

    # Half this corpus has Amharic filenames, and the default Windows console
    # encoding cannot represent them.
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")
        except (AttributeError, OSError):
            pass

    ocr_lang = args.ocr_lang or ("amh+eng" if args.language == "am" else "eng")
    args.out.mkdir(parents=True, exist_ok=True)

    exit_code = 0
    for pdf_path in args.pdfs:
        if not pdf_path.is_file():
            print(f"error: {pdf_path} is not a file", file=sys.stderr)
            exit_code = 1
            continue

        try:
            payload, report = extract(pdf_path, args.language, args.ocr, ocr_lang,
                                      allow_low_quality=args.allow_low_quality)
        except LowQualityText as exc:
            print(f"\nREFUSED {exc}", file=sys.stderr)
            exit_code = 3
            continue

        out_path = args.out / f"{pdf_path.stem}.json"
        out_path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")

        report.print_to(sys.stderr)
        print(f"  → {out_path}", file=sys.stderr)

        if report.fallback:
            # Non-zero so a batch run cannot quietly produce a corpus that is
            # mostly window-chunked.
            exit_code = 2

    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
