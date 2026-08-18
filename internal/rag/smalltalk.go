package rag

import (
	"strings"
	"unicode"
)

// Intent is what a message is actually asking for.
//
// Only two things happen with it: a message that is plainly not a legal question
// gets a written reply, and everything else goes through retrieval. The default
// is IntentLegal, and every rule below is a whole-message or exact-phrase match,
// because the cost of the two mistakes is not symmetric. Sending a greeting to
// retrieval produces an awkward "I could not find this in the corpus". Sending a
// legal question to a canned reply produces an answer about Ethiopian law that
// no source supports, which is the one thing this system must never do.
type Intent string

const (
	// IntentLegal is anything that might be a question about the law. The default.
	IntentLegal Intent = "legal"

	IntentGreeting   Intent = "greeting"
	IntentThanks     Intent = "thanks"
	IntentFarewell   Intent = "farewell"
	IntentCapability Intent = "capability"
)

// Whole-message matches. "hi" is a greeting; "hi, what does article 5 say?" is a
// legal question with a greeting attached, and belongs in retrieval.
var (
	greetings = set(
		"hello", "hello there", "hi", "hi there", "hiya", "hey", "hey there",
		"good morning", "good afternoon", "good evening", "greetings",
		"selam", "salam", "ሰላም", "ሰላም ነው",
	)

	thanks = set(
		"thanks", "thank you", "thanks a lot", "thank you very much",
		"thanks so much", "thx", "cheers", "much appreciated", "appreciated",
		"አመሰግናለሁ", "እናመሰግናለን",
	)

	farewells = set(
		"bye", "goodbye", "bye bye", "see you", "see you later", "good night",
	)
)

// capabilityPhrases are asked in enough different ways that whole-message
// matching is too strict, so these match as substrings — but only in a short
// message, so "what can you do about a breach of Article 12" stays a legal
// question.
var capabilityPhrases = []string{
	"what can you do",
	"what do you do",
	"what can i ask",
	"what can you help with",
	"how can you help",
	"how do you work",
	"how does this work",
	"what are you",
	"who are you",
	"what is seera",
	"who made you",
	"are you a lawyer",
	"are you an ai",
	"what sources do you use",
	"where do your answers come from",
}

const maxCapabilityRunes = 80

// Classify decides how a message should be handled.
func Classify(text string) Intent {
	normal := normalise(text)
	if normal == "" {
		return IntentLegal
	}

	switch {
	case greetings[normal]:
		return IntentGreeting
	case thanks[normal]:
		return IntentThanks
	case farewells[normal]:
		return IntentFarewell
	}

	if len([]rune(normal)) <= maxCapabilityRunes {
		for _, phrase := range capabilityPhrases {
			if strings.Contains(normal, phrase) {
				return IntentCapability
			}
		}
	}

	return IntentLegal
}

// ConversationalReply is what Seera says to a message that is not a legal
// question.
//
// Written here rather than generated. These replies describe the product and
// its limits, which is exactly the text that must not vary run to run or drift
// into a claim about the law — and answering them without a model call costs
// nothing and returns instantly.
func ConversationalReply(intent Intent) string {
	switch intent {
	case IntentGreeting:
		return `Hello. I answer questions about Ethiopian law from indexed legal sources, and I show the article behind every statement so you can check it.

Ask me something specific — "what are the requirements for forming a private limited company?", or an article number directly if you know one.`

	case IntentThanks:
		return `You are welcome. Ask another question whenever you need one.`

	case IntentFarewell:
		return `Goodbye. This conversation is saved in your browser, so you can pick it up again from History.`

	case IntentCapability:
		return `I search a corpus of Ethiopian legal documents — codes, proclamations, regulations and directives, in English and Amharic — and answer from what I find there.

What I can do:

- Quote a provision and explain in plain language what it says and what follows from it.
- Show the document, article and version behind every statement, with the date that version came into force.
- Answer in either English or Amharic, from sources in either language.
- Tell you what the law was on a date you name, rather than only today.

What I will not do: guess. If the corpus has nothing on point, I say so instead of producing something plausible. Coverage is still partial, so an answer being absent here does not mean the law is silent.

I am not a lawyer and this is not legal advice.`

	default:
		return ""
	}
}

// normalise lowercases and strips punctuation, spacing and emoji, so "Hello!!"
// and "hello" are the same message.
func normalise(text string) string {
	var b strings.Builder
	lastSpace := true

	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		case unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
			// Collapse any run of separators into one space.
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
		}
	}

	return strings.TrimSpace(b.String())
}

func set(values ...string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}
