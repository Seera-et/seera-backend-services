package rag

import (
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// Found live, 2026-08-31: MaxTurnRunes at 1200 truncated a long structured
// answer before a lettered section ("B. Formation of a One Member PLC") the
// reader then asked about ("tell me more about... under B"), so generation
// never saw what "B" referred to and the follow-up abstained. MaxTurnRunes
// was raised to 4000; these lock in that a realistic long answer's later
// content now survives.
func TestTrimHistoryKeepsLateContentWithinRaisedRuneCap(t *testing.T) {
	long := strings.Repeat("filler ", 500) + "B. Formation of a One Member PLC — MARKER"

	trimmed := TrimHistory([]Turn{{Role: ai.RoleAssistant, Text: long}})

	if len(trimmed) != 1 || !strings.Contains(trimmed[0].Text, "MARKER") {
		t.Error("a marker placed past the old 1200-rune cap did not survive TrimHistory")
	}
}

func TestWriteHistoryKeepsLateContentWithinRaisedRuneCap(t *testing.T) {
	long := strings.Repeat("filler ", 500) + "B. Formation of a One Member PLC — MARKER"

	var b strings.Builder
	writeHistory(&b, []Turn{{Role: ai.RoleAssistant, Text: long}})

	if !strings.Contains(b.String(), "MARKER") {
		t.Error("a marker placed past the old 1200-rune cap did not survive writeHistory")
	}
}
