package rag

import (
	"regexp"
	"strconv"
	"strings"
)

// numberPattern matches a digit sequence, with optional thousands separators
// and a decimal point — "15,000", "15000", "3.5" all match, each as one span.
var numberPattern = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

// numbersIn extracts every distinct number in text — digit form and spelled
// out in English words alike — normalised by stripping thousands separators
// so "15,000" and "15000" compare equal, and by resolving word forms so
// "fifteen thousand" and "15000" compare equal too.
//
// The word form matters more than it might look: this corpus spells out
// nearly every amount in prose ("fifteen thousand Ethiopian Birr", "ninety
// consecutive days") rather than digits, confirmed live 2026-09-03 — Article
// 496 contains zero digit characters at all. Digit-only extraction would
// make lexicallySupported unable to check almost any real citation in this
// corpus, either flagging every correctly-restated figure as unsupported (the
// model routinely writes "fifteen thousand (15,000)") or, if a source with no
// digits were made to pass unconditionally, doing nothing for most of the
// corpus at all.
func numbersIn(text string) map[string]bool {
	out := make(map[string]bool)
	for _, m := range numberPattern.FindAllString(text, -1) {
		out[strings.ReplaceAll(m, ",", "")] = true
	}
	for n := range wordNumbersIn(text) {
		out[n] = true
	}
	return out
}

// numberWords covers the English cardinals this corpus's amounts, day
// counts, and member counts are actually built from. Not a general English
// numeral parser — bounded to what a legal amount or duration plausibly
// spells out, the same scoping GeezToInt (internal/search/service.go) applies
// to Ge'ez numerals for the same reason: good enough for the domain, not a
// claim to handle arbitrary text.
var numberWords = map[string]int{
	"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

// wordNumbersIn scans text for runs of English number words and resolves
// each run to one integer, mirroring GeezToInt's group-accumulation shape:
// "hundred" multiplies the group so far, "thousand" multiplies everything
// accumulated so far and starts a new group. "two hundred thirty four
// thousand" therefore resolves to 234000, and a run ends (and is emitted) as
// soon as a non-number word breaks it.
func wordNumbersIn(text string) map[string]bool {
	out := make(map[string]bool)
	group, total := 0, 0
	inRun := false

	flush := func() {
		if inRun {
			out[strconv.Itoa(total+group)] = true
		}
		group, total, inRun = 0, 0, false
	}

	for _, w := range strings.Fields(strings.ToLower(text)) {
		w = strings.Trim(w, ".,;:()\"'")
		switch {
		case w == "hundred" && inRun:
			if group == 0 {
				group = 1
			}
			group *= 100
		case w == "thousand" && inRun:
			if group == 0 {
				group = 1
			}
			total += group * 1000
			group = 0
		default:
			if v, ok := numberWords[w]; ok {
				group += v
				inRun = true
			} else {
				flush()
			}
		}
	}
	flush()
	return out
}

// lexicallySupported reports whether every number the claim states also
// appears somewhere in the source.
//
// Free and deterministic — no model call — so unlike Verifier this runs
// unconditionally, on every answer. It is a narrow, high-precision check for
// one specific failure mode: a wrong or invented figure, amount, deadline,
// or threshold, exactly what SystemPrompt rule 3 already tells the model not
// to smooth over. A claim with no numbers has nothing this check can verify,
// so it passes by construction — this is not a general entailment judgment
// (that stays Verifier's job), only a cheap guard against the single most
// mechanically-checkable way a citation can misstate its source.
func lexicallySupported(claim, source string) bool {
	claimNumbers := numbersIn(claim)
	if len(claimNumbers) == 0 {
		return true
	}
	sourceNumbers := numbersIn(source)
	for n := range claimNumbers {
		if !sourceNumbers[n] {
			return false
		}
	}
	return true
}
