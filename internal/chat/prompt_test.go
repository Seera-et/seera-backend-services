package chat

import (
	"strings"
	"testing"
)

func TestSystemPromptForUnknownCategoryIsUnchanged(t *testing.T) {
	if got := systemPromptFor(""); got != systemPrompt {
		t.Error("systemPromptFor(\"\") must be byte-identical to the base prompt")
	}
	if got := systemPromptFor(Category("something_new")); got != systemPrompt {
		t.Error("an unrecognized category must fall back to the base prompt unchanged")
	}
}

// The safety rule must survive concatenation for every category — a guidance
// string that accidentally overrode or obscured it would be invisible to any
// test that only checks the guidance itself.
func TestEveryCategoryPromptKeepsTheHardRule(t *testing.T) {
	const hardRule = "Never state what Ethiopian law says"

	for category := range categoryGuidance {
		if !strings.Contains(systemPromptFor(category), hardRule) {
			t.Errorf("category %q lost the hard rule after concatenation", category)
		}
	}
}
