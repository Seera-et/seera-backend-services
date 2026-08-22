package router

import (
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// CleanRewrite normalises the model's rewritten question and rejects anything
// that does not look like one.
//
// A rewrite that came back as a paragraph, an answer, or an empty string is
// discarded: searching for the model's prose instead of the user's question is
// worse than searching for the original.
func CleanRewrite(raw, original string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}

	// Keep the first line. Anything after it is commentary the prompt asked for
	// and did not get.
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	text = strings.TrimSpace(strings.Trim(text, `"'“”`))
	if text == "" {
		return ""
	}

	// Three times the original is not a rewrite of it.
	if limit := 3*len([]rune(original)) + 200; len([]rune(text)) > limit {
		return ""
	}
	return text
}

// JoinForRetrieval is the query to search for when no rewrite is available: the
// most recent user turn followed by the current question, so the terms a pronoun
// stands in for are at least present for lexical and vector search to find.
//
// Worse than a real rewrite, and much better than searching for "what about for
// a PLC?" on its own.
func JoinForRetrieval(in Input) string {
	question := strings.TrimSpace(in.Question)

	for i := len(in.History) - 1; i >= 0; i-- {
		if in.History[i].Role != ai.RoleUser {
			continue
		}
		previous := strings.TrimSpace(in.History[i].Text)
		if previous == "" {
			continue
		}
		return previous + "\n" + question
	}

	return question
}
