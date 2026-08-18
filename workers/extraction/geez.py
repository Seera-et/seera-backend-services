"""Ge'ez (Ethiopic) numeral handling.

Amharic legal documents print article numbers in Ge'ez numerals: ``አንቀጽ ፵፭`` is
Article 45. Retrieval and citations work in Arabic numerals, so every number is
normalised on the way in — while the printed form is preserved alongside it, so
a user who searches the way the document is written still finds the article.

Getting this wrong is not a cosmetic bug. A mis-parsed numeral silently attaches
an answer to the wrong provision, and nothing downstream can detect it.
"""

from __future__ import annotations

# ፩–፱
UNITS = {
    "፩": 1, "፪": 2, "፫": 3, "፬": 4, "፭": 5,
    "፮": 6, "፯": 7, "፰": 8, "፱": 9,
}

# ፲–፺
TENS = {
    "፲": 10, "፳": 20, "፴": 30, "፵": 40, "፶": 50,
    "፷": 60, "፸": 70, "፹": 80, "፺": 90,
}

HUNDRED = "፻"   # ፻
TEN_THOUSAND = "፼"  # ፼

_VALUES = {**UNITS, **TENS, HUNDRED: 100, TEN_THOUSAND: 10_000}


def is_geez_numeral(text: str) -> bool:
    """True when every character is an Ethiopic numeral."""
    return bool(text) and all(ch in _VALUES for ch in text)


def geez_to_int(text: str) -> int | None:
    """Convert a Ge'ez numeral to an int, or None if it is not one.

    Ethiopic numerals are positional by multiplier rather than by place: units
    and tens add within a group, ፻ multiplies the group before it by 100, and ፼
    multiplies everything so far by 10,000. So ፪፻፴፬ is (2 × 100) + 30 + 4 = 234.
    """
    if not is_geez_numeral(text):
        return None

    total = 0
    group = 0

    for ch in text:
        value = _VALUES[ch]
        if value == 100:
            # A bare ፻ means one hundred, not zero hundreds.
            total += (group or 1) * 100
            group = 0
        elif value == 10_000:
            total = ((total + group) or 1) * 10_000
            group = 0
        else:
            group += value

    return total + group


# Amharic legal drafters number structural divisions with words rather
# than numerals — "ምዕራፍ አንድ" is Chapter One, never "ምዕራፍ ፩". Both cardinal and
# ordinal forms appear, and the ordinals take a definite suffix ("ሁለተኛው መጽሐፍ",
# the second book), so all three spellings are mapped.
AMHARIC_NUMBER_WORDS = {
    "አንድ": 1, "አንደኛ": 1, "አንደኛው": 1,
    "ሁለት": 2, "ሁለተኛ": 2, "ሁለተኛው": 2,
    "ሦስት": 3, "ሶስት": 3, "ሦስተኛ": 3, "ሶስተኛ": 3, "ሦስተኛው": 3, "ሶስተኛው": 3,
    "አራት": 4, "አራተኛ": 4, "አራተኛው": 4,
    "አምስት": 5, "አምስተኛ": 5, "አምስተኛው": 5,
    "ስድስት": 6, "ስድስተኛ": 6, "ስድስተኛው": 6,
    "ሰባት": 7, "ሰባተኛ": 7, "ሰባተኛው": 7,
    "ስምንት": 8, "ስምንተኛ": 8, "ስምንተኛው": 8,
    "ዘጠኝ": 9, "ዘጠነኛ": 9, "ዘጠነኛው": 9,
    "አስር": 10, "አስረኛ": 10, "አስረኛው": 10,
    "አስራ አንድ": 11, "አስራ ሁለት": 12, "አስራ ሦስት": 13, "አስራ ሶስት": 13,
    "አስራ አራት": 14, "አስራ አምስት": 15, "አስራ ስድስት": 16, "አስራ ሰባት": 17,
    "አስራ ስምንት": 18, "አስራ ዘጠኝ": 19, "ሃያ": 20,
}


def normalize_number(raw: str) -> str:
    """Return the Arabic-numeral form of an article number.

    Accepts Ge'ez numerals, Arabic numerals, and Arabic numerals with a letter
    suffix (``45a``, common in amended codes). Anything else is returned
    stripped and unchanged rather than guessed at — a number this function does
    not understand should reach a human, not become a wrong citation.
    """
    raw = raw.strip()
    if not raw:
        return raw

    if raw in AMHARIC_NUMBER_WORDS:
        return str(AMHARIC_NUMBER_WORDS[raw])

    value = geez_to_int(raw)
    if value is not None:
        return str(value)

    # Mixed Ge'ez with a Latin suffix, e.g. "፵፭a".
    digits = "".join(ch for ch in raw if ch in _VALUES)
    if digits and len(digits) < len(raw):
        suffix = "".join(ch for ch in raw if ch not in _VALUES)
        value = geez_to_int(digits)
        if value is not None and suffix.isalpha():
            return f"{value}{suffix}"

    return raw
