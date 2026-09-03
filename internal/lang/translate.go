// Package lang detects Ethiopic-script text and translates it to English for
// callers that need to reason about a query in English — routing
// classification and retrieval both hit the same problem (a model or index
// tuned on English-language cues sees a script it wasn't trained to weigh
// correctly) and both fix it the same way, so the detector and the one-call
// translation live here once instead of twice.
package lang

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// EthiopicScript matches any character in the Ethiopic Unicode block
// (U+1200-U+137F), which Amharic, Ge'ez, and Tigrinya all share. A script
// check, not a language classifier — good enough to gate an optional
// translation step, not a claim about which of the three it actually is.
var EthiopicScript = regexp.MustCompile(`[\x{1200}-\x{137F}]`)

// HasEthiopicScript reports whether text contains any Ethiopic-block
// character.
func HasEthiopicScript(text string) bool {
	return EthiopicScript.MatchString(text)
}

const (
	// Timeout bounds the one translation call. Callers sit in front of a
	// latency-sensitive path (routing, retrieval), so a slow provider must
	// not hold up the rest of the request over this optional step.
	Timeout = 4 * time.Second

	maxTokens = 200
)

const prompt = `Translate the message to English. Output only the translation — no quotes, no preamble, no explanation. If it is already in English, repeat it unchanged.`

// Translate is the one LLM call a caller adds to bridge Ethiopic-script text
// to English. Kept to a single, small, non-streaming call — the same cost
// shape as the router's own follow-up rewrite, not a new category of expense.
func Translate(ctx context.Context, llm ai.LLM, text string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	raw, _, err := llm.Complete(ctx, ai.Request{
		System:    prompt,
		Messages:  []ai.Message{{Role: ai.RoleUser, Content: text}},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}
