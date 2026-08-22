package chat

import "testing"

// The guard has to let the correct answer through and stop the dangerous one.
// Both directions are tested, because a guard that trips on everything is a
// guard that gets removed.
func TestAssertsLaw(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		// Allowed: this is exactly what the prompt asks the engine to produce.
		{
			name: "the required disclaimer mentions Ethiopian law without asserting it",
			text: "In general terms, a private limited company is one where the owners' liability is limited to their contribution. That is the general concept and not Ethiopian law. Shall I search the corpus for how Ethiopian law defines it?",
			want: false,
		},
		{
			name: "greeting",
			text: "Hello. I answer questions about Ethiopian law from indexed sources. What would you like to know?",
			want: false,
		},
		{
			name: "capability description",
			text: "I search a corpus of Ethiopian legal documents and cite the article behind every statement. Coverage is partial, and I say so when nothing is found.",
			want: false,
		},
		{
			name: "offer to search, phrased with a deontic word",
			text: "I would need to search the corpus to tell you what Ethiopian law requires here. Ask me and I will.",
			want: false,
		},
		{
			name: "general concept with no jurisdiction",
			text: "Liability means responsibility for a legal obligation. A limited company caps how much of it the owners carry.",
			want: false,
		},

		// Blocked.
		{
			name: "invents an article number",
			text: "Under the general principle, registration is required by Article 265.",
			want: true,
		},
		{
			name: "invents a proclamation",
			text: "That is governed by Proclamation No. 1243/2021 in general terms.",
			want: true,
		},
		{
			name: "names a specific instrument",
			text: "The Commercial Code deals with company formation.",
			want: true,
		},
		{
			name: "states an obligation under Ethiopian law",
			text: "In Ethiopia you must register the company before trading.",
			want: true,
		},
		{
			name: "states a deadline",
			text: "Ethiopian law requires the filing within 30 days of formation.",
			want: true,
		},
		{
			name: "Amharic article reference",
			text: "አንቀጽ 265 ይህንን ይደነግጋል።",
			want: true,
		},
		{
			name: "assertion in one sentence is not excused by an offer in another",
			text: "In Ethiopia a company must have two founders. Shall I search the corpus to confirm?",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, got := AssertsLaw(tt.text)
			if got != tt.want {
				t.Errorf("AssertsLaw() = %v (%q), want %v\ntext: %s",
					got, reason, tt.want, tt.text)
			}
			if got && reason == "" {
				t.Error("a blocked reply must carry a reason for the log")
			}
		})
	}
}
