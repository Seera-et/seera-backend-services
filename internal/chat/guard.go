package chat

import (
	"regexp"
	"strings"
)

// The guard: the last thing between a reply written from memory and a reader who
// will reasonably assume it came from the law.
//
// It looks for two things a general reply must never contain.
//
// A citation. A reply with no sources attached that names "Article 265" has
// invented a reference — precisely the model-authored citation the whole system
// is built to prevent. This check is nearly exact and trips hard.
//
// An assertion about Ethiopian law. Harder, because the prompt *requires* the
// reply to mention Ethiopian law — "that is the general concept, not Ethiopian
// law; shall I search the corpus?" is the correct answer, not a violation. So a
// mention is not enough: a sentence has to tie Ethiopia to an obligation, and
// not be an offer or a question, before it counts.
//
// The asymmetry is deliberate. A false positive costs a redundant retrieval and
// a slower answer. A false negative puts an uncited legal claim on the screen.
var (
	// "Article 12", "Art. 5(2)", "Proclamation No. 1243/2021", "አንቀጽ ፵፭".
	//
	// The optional "No." matters: the printed form of an Ethiopian instrument
	// almost always carries it, and without this the most citation-looking string
	// of all would pass.
	citationPattern = regexp.MustCompile(
		`(?i)\b(article|art\.?|proclamation|regulation|directive)\s*(no\.?|number|№)?\s*[0-9፩-፼]` +
			`|አንቀጽ\s*[0-9፩-፼]`)

	// Naming a specific instrument is the same problem in different clothing.
	instrumentPattern = regexp.MustCompile(
		`(?i)\b(commercial code|civil code|criminal code|labour proclamation|labor proclamation)\b`)

	ethiopiaPattern = regexp.MustCompile(`(?i)\bethiopian?\b|ኢትዮጵያ`)

	// Obligation, permission, prohibition, entitlement, deadlines, money.
	deonticPattern = regexp.MustCompile(
		`(?i)\b(must|shall|has to|have to|required to|requires?|obliged|mandatory|` +
			`prohibited|forbidden|not allowed|may not|cannot legally|entitled to|` +
			`liable|penalt(y|ies)|fined?|within \d+\s*(day|month|year)|minimum capital)\b`)

	// An offer to look something up, or a question, is not an assertion.
	offerPattern = regexp.MustCompile(
		`(?i)\b(shall i|should i|would you like|do you want|i can search|i can look|` +
			`let me search|i will search|ask me|if you ask|you can ask|i would need to)\b`)
)

// AssertsLaw reports whether a reply reads as a statement about Ethiopian law,
// with a short reason for the log.
func AssertsLaw(text string) (string, bool) {
	if citationPattern.MatchString(text) {
		return "cites an article or proclamation number", true
	}
	if instrumentPattern.MatchString(text) {
		return "names a specific Ethiopian legal instrument", true
	}

	for _, sentence := range splitSentences(text) {
		if offerPattern.MatchString(sentence) || strings.Contains(sentence, "?") {
			continue
		}
		if ethiopiaPattern.MatchString(sentence) && deonticPattern.MatchString(sentence) {
			return "states an obligation under Ethiopian law", true
		}
	}

	return "", false
}

// splitSentences is a rough split on terminators, including the Ethiopic full
// stop. Good enough to keep an offer in one sentence from tainting a statement
// in another.
//
// The terminator stays attached, because a trailing question mark is one of the
// signals that a sentence is not an assertion.
var sentencePattern = regexp.MustCompile(`[^.!?።\n]+[.!?።]?`)

func splitSentences(text string) []string {
	matches := sentencePattern.FindAllString(text, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		if trimmed := strings.TrimSpace(match); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
