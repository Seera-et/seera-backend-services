package rag

import (
	"strings"
	"testing"
)

// The gate has one job on the happy path — be invisible — and one job on the
// other path: remove the sentinel completely, before any of it is shown.
func TestPrefixGate(t *testing.T) {
	tests := []struct {
		name        string
		chunks      []string
		wantText    string
		wantMatched bool
	}{
		{
			name:     "ordinary answer passes through unchanged",
			chunks:   []string{"A private limited ", "company may be formed [S1]."},
			wantText: "A private limited company may be formed [S1].",
		},
		{
			name:        "sentinel arriving whole",
			chunks:      []string{"NOT_IN_SOURCES\nThe sources do not settle this."},
			wantText:    "The sources do not settle this.",
			wantMatched: true,
		},
		{
			name:        "sentinel split across chunks",
			chunks:      []string{"NOT_", "IN_", "SOURCES", ": these provisions only touch on it."},
			wantText:    "these provisions only touch on it.",
			wantMatched: true,
		},
		{
			name:        "sentinel one character at a time",
			chunks:      strings.Split("NOT_IN_SOURCESnope", ""),
			wantText:    "nope",
			wantMatched: true,
		},
		{
			name:        "leading whitespace before the sentinel",
			chunks:      []string{"\n\n", "NOT_IN_SOURCES — nothing on point."},
			wantText:    "nothing on point.",
			wantMatched: true,
		},
		{
			name: "an answer that begins with the same letters",
			// "NOT" is a plausible start to a real sentence, so the gate must
			// release it once it diverges rather than swallowing the opening.
			chunks:   []string{"NOT", "HING in these provisions requires it [S1]."},
			wantText: "NOTHING in these provisions requires it [S1].",
		},
		{
			name:     "a short answer that never reaches the sentinel length",
			chunks:   []string{"No."},
			wantText: "No.",
		},
		{
			name:     "empty stream",
			chunks:   nil,
			wantText: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := newPrefixGate(InsufficientSourcesSentinel)

			var out strings.Builder
			for _, chunk := range tt.chunks {
				out.WriteString(gate.Write(chunk))
			}
			out.WriteString(gate.Close())

			if got := out.String(); got != tt.wantText {
				t.Errorf("text = %q, want %q", got, tt.wantText)
			}
			if gate.Matched() != tt.wantMatched {
				t.Errorf("matched = %v, want %v", gate.Matched(), tt.wantMatched)
			}
			// Whatever happens, the control token must not reach the reader.
			if strings.Contains(out.String(), InsufficientSourcesSentinel) && tt.wantMatched {
				t.Error("the sentinel leaked into the answer")
			}
		})
	}
}

func TestAnswerReportsInsufficientSources(t *testing.T) {
	svc := newTestService(
		&stubRetriever{results: testResults(3)},
		&stubLLM{chunks: []string{
			"NOT_IN_SOURCES\n",
			"The sources do not contain the requirements for a business licence. ",
			"They state only that specific requirements may be imposed [S1].",
		}},
		PassthroughReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "What are the requirements for a business licence?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	var answer strings.Builder
	for _, e := range events {
		if e.Type == EventToken {
			answer.WriteString(e.Text)
		}
	}

	if strings.Contains(answer.String(), InsufficientSourcesSentinel) {
		t.Errorf("the sentinel reached the client:\n%s", answer.String())
	}
	if !strings.HasPrefix(answer.String(), "The sources do not contain") {
		t.Errorf("answer begins %q, want the sentinel stripped", answer.String())
	}

	done := events[len(events)-1]
	if done.Result.Kind != KindInsufficient {
		t.Errorf("kind = %q, want %q — the reader must not be told this was answered",
			done.Result.Kind, KindInsufficient)
	}
	// The sources were read and cited; they are worth showing.
	if len(done.Result.Citations) == 0 {
		t.Error("the provisions that were consulted were dropped")
	}
}

func TestAnswerWithoutTheSentinelIsStillLegal(t *testing.T) {
	svc := newTestService(
		&stubRetriever{results: testResults(2)},
		&stubLLM{chunks: []string{"A company must register [S1]."}},
		PassthroughReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "Must a company register?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if kind := events[len(events)-1].Result.Kind; kind != KindLegal {
		t.Errorf("kind = %q, want %q", kind, KindLegal)
	}
}
