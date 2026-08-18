"""Tests for the parts of extraction that fail silently when wrong.

    python -m unittest discover -s workers/extraction

Numeral conversion and article detection are tested because a mistake in either
produces a corpus that looks fine and cites the wrong law. PDF handling is not
tested here — it needs real documents, and its failure mode is visible in the
per-document report.
"""

from __future__ import annotations

import unittest

from extract import build_articles, build_windows, find_structure
from geez import geez_to_int, is_geez_numeral, normalize_number


class TestGeezNumerals(unittest.TestCase):
    def test_units_and_tens(self):
        for text, want in [("፩", 1), ("፱", 9), ("፲", 10), ("፲፪", 12), ("፳", 20), ("፺፱", 99)]:
            with self.subTest(text=text):
                self.assertEqual(geez_to_int(text), want)

    def test_hundreds(self):
        for text, want in [("፻", 100), ("፪፻", 200), ("፪፻፴፬", 234), ("፱፻፺፱", 999)]:
            with self.subTest(text=text):
                self.assertEqual(geez_to_int(text), want)

    def test_ten_thousands(self):
        self.assertEqual(geez_to_int("፼"), 10_000)

    def test_article_numbers_seen_in_practice(self):
        # ፵፭ is Article 45 — the plan's worked example.
        self.assertEqual(geez_to_int("፵፭"), 45)

    def test_rejects_non_numerals(self):
        for text in ["", "45", "አንቀጽ", "፵a"]:
            with self.subTest(text=text):
                self.assertIsNone(geez_to_int(text))
                self.assertFalse(is_geez_numeral(text))

    def test_normalize_number(self):
        cases = [
            ("፵፭", "45"),
            ("45", "45"),
            ("45a", "45a"),
            (" ፲፪ ", "12"),
            ("፵፭a", "45a"),
        ]
        for raw, want in cases:
            with self.subTest(raw=raw):
                self.assertEqual(normalize_number(raw), want)

    def test_unrecognised_input_is_returned_unchanged(self):
        # Better a number a human has to look at than a confidently wrong one.
        self.assertEqual(normalize_number("Art-45/2"), "Art-45/2")


ENGLISH_DOC = """CHAPTER 3
Companies

Article 45 Formation of private limited companies
A private limited company may be formed by two or more persons.

Article 46 Capital
(1) The capital shall not be less than the prescribed minimum.
(2) The period shall be thirty days.

CHAPTER 4
Dissolution

Article 47 Grounds
A company is dissolved where...
"""

AMHARIC_DOC = """ምዕራፍ ፫
ስለ ኩባንያዎች

አንቀጽ ፵፭ ስለ ኩባንያ ምስረታ
ኩባንያ በሁለት ወይም ከዚያ በላይ በሆኑ ሰዎች ሊመሠረት ይችላል።

አንቀጽ ፵፮ ስለ ካፒታል
የኩባንያው ካፒታል ከተወሰነው መጠን ማነስ የለበትም።
"""


class TestArticleDetection(unittest.TestCase):
    def test_english_articles_and_section_path(self):
        articles = build_articles(ENGLISH_DOC, [0], "en")

        self.assertEqual([a.article_no for a in articles], ["45", "46", "47"])
        self.assertEqual(articles[0].title, "Formation of private limited companies")
        self.assertEqual(articles[0].section_path, ["Chapter 3 — Companies"])
        # The chapter heading between 46 and 47 must not be swallowed into 46.
        self.assertNotIn("CHAPTER 4", articles[1].text)
        self.assertEqual(articles[2].section_path, ["Chapter 4 — Dissolution"])

    def test_article_text_stops_at_the_next_article(self):
        articles = build_articles(ENGLISH_DOC, [0], "en")
        self.assertIn("thirty days", articles[1].text)
        self.assertNotIn("Article 47", articles[1].text)

    def test_amharic_articles_are_normalised(self):
        articles = build_articles(AMHARIC_DOC, [0], "am")

        self.assertEqual([a.article_no for a in articles], ["45", "46"])
        # The printed form is preserved so a reader searching it still matches.
        self.assertEqual([a.article_no_raw for a in articles], ["፵፭", "፵፮"])
        # Ge'ez numerals are normalised in headings too, and the heading's title
        # is picked up from the line below it.
        self.assertEqual(articles[0].section_path, ["Chapter 3 — ስለ ኩባንያዎች"])

    def test_repeated_article_numbers_are_dropped(self):
        # Running headers and tables of contents repeat article markers; the
        # first occurrence is the provision, the rest are noise.
        doc = ENGLISH_DOC + "\nArticle 45 Formation of private limited companies\n"
        articles = build_articles(doc, [0], "en")
        self.assertEqual([a.article_no for a in articles], ["45", "46", "47"])

    def test_a_higher_heading_clears_the_levels_below_it(self):
        # Without this, every later article's path lists every chapter the
        # document has ever had, and the context header embedded with each
        # chunk fills up with provenance that is no longer true.
        doc = (
            "BOOK I\nPersons\n\n"
            "CHAPTER 1\nCapacity\n\n"
            "Article 1 Scope\nText.\n\n"
            "BOOK II\nProperty\n\n"
            "Article 2 Ownership\nText.\n"
        )
        articles = build_articles(doc, [0], "en")

        # Roman numerals are normalised to Arabic, so a section path reads the
        # same however the document happened to number its divisions.
        self.assertEqual(articles[0].section_path, ["Book 1 — Persons", "Chapter 1 — Capacity"])
        self.assertEqual(articles[1].section_path, ["Book 2 — Property"])

    def test_word_numbered_uppercase_headings(self):
        # How the 2021 Commercial Code actually numbers its divisions: capitals
        # and English words, never "CHAPTER 1" or "CHAPTER I".
        doc = (
            "BOOK TWO\nBUSINESS ORGANIZATIONS\n\n"
            "TITLE SIX\nSHARE COMPANY\n\n"
            "Article 1 Scope\nText.\n"
        )
        articles = build_articles(doc, [0], "en")

        self.assertEqual(articles[0].section_path,
                         ["Book 2 — BUSINESS ORGANIZATIONS", "Title 6 — SHARE COMPANY"])

    def test_prose_cross_references_are_not_headings(self):
        # Both of these appear in the real document and both used to parse as
        # headings, replacing the section path of every article beneath them.
        # "Book is..." was read as Book I with the title "s to protect...".
        doc = (
            "BOOK ONE\nGENERAL PROVISIONS\n\n"
            "Article 1 Scope\n"
            "Book II of this Code applicable to business organizations.\n"
            "Book is to protect the legitimate interests of creditors.\n"
            "Title IV of this Code.\n"
        )
        articles = build_articles(doc, [0], "en")

        self.assertEqual(len(articles), 1)
        self.assertEqual(articles[0].section_path, ["Book 1 — GENERAL PROVISIONS"])

    def test_heading_title_is_not_taken_from_a_sentence(self):
        doc = "CHAPTER ONE\nof this Code applicable to traders.\n\nArticle 1 Scope\nText.\n"
        articles = build_articles(doc, [0], "en")

        # No title rather than a sentence fragment: the section path rides along
        # in the context header of every chunk beneath the heading.
        self.assertEqual(articles[0].section_path, ["Chapter 1"])

    def test_markers_split_across_two_lines_are_found(self):
        # PDF extraction follows page layout, so a marker at a line or column
        # break arrives as "Article\n86". In the Amharic commercial code this is
        # 42 of 825 articles.
        doc = (
            "Article\n86\nSummary Registration\nThe registrar shall...\n\n"
            "አንቀጽ\n13\nየንግድ መዝገብ\nመዝጋቢው...\n"
        )
        self.assertEqual([a.article_no for a in build_articles(doc, [0], "en")], ["86"])
        self.assertEqual([a.article_no for a in build_articles(doc, [0], "am")], ["13"])

    def test_marker_without_a_space_is_found(self):
        # "Article86." appears in the 2021 Commercial Code.
        articles = build_articles("Article86. Summary Registration\nThe registrar...\n", [0], "en")
        self.assertEqual([a.article_no for a in articles], ["86"])

    def test_cross_reference_split_across_lines_is_not_an_article(self):
        # The anchoring that makes split markers safe: a wrapped cross-reference
        # must not start a phantom article.
        doc = "Article 1 Scope\nThe provisions of this Article\n425 shall apply.\n"
        self.assertEqual([a.article_no for a in build_articles(doc, [0], "en")], ["1"])

    def test_duplicate_numbers_keep_the_longest_body(self):
        # A table of contents lists every article before the provisions begin.
        # Taking the first occurrence takes the table of contents.
        doc = (
            "Article 45 Formation\n"
            "Article 46 Capital\n\n"
            "Article 45 Formation\n"
            "A private limited company may be formed by two or more persons, "
            "and shall have a capital of not less than the prescribed minimum.\n"
        )
        articles = build_articles(doc, [0], "en")

        by_number = {a.article_no: a for a in articles}
        self.assertIn("may be formed", by_number["45"].text)

    def test_structure_markers_are_in_document_order(self):
        offsets = [m[0] for m in find_structure(ENGLISH_DOC, "en")]
        self.assertEqual(offsets, sorted(offsets))

    def test_confidence_is_high_for_detected_articles(self):
        for article in build_articles(ENGLISH_DOC, [0], "en"):
            self.assertEqual(article.structure_confidence, "high")


class TestWindowFallback(unittest.TestCase):
    def test_windows_overlap_and_are_marked_low_confidence(self):
        text = "x" * 6000
        windows = build_windows(text, [0])

        self.assertGreater(len(windows), 1)
        for w in windows:
            self.assertEqual(w.structure_confidence, "low")
            self.assertTrue(w.article_no.startswith("window-"))

        # Overlap exists, so a provision straddling a boundary appears whole in
        # at least one window.
        self.assertLess(windows[1].char_start, windows[0].char_end)

    def test_window_numbers_are_unique(self):
        windows = build_windows("y" * 20000, [0])
        numbers = [w.article_no for w in windows]
        self.assertEqual(len(numbers), len(set(numbers)))


if __name__ == "__main__":
    unittest.main()
