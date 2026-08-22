//go:build eval

// Package evaluation measures the answer path against a fixed dataset.
//
// Behind the `eval` build tag: these tests call a real provider, cost money and
// quota, and take seconds per case. `go test ./...` stays offline and fast.
//
//	set -a && source .env && set +a
//	go test -tags eval ./tests/evaluation/... -v
package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/router"
)

// result is one case's outcome, written to the run report.
type result struct {
	ID       string `json:"id"`
	Message  string `json:"message"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
	Rewrite  string `json:"rewrite,omitempty"`
	Correct  bool   `json:"correct"`

	// ExpectedCategory/GotCategory are tracked separately from Correct: a
	// category miss is a tone/UX issue, never a safety issue, so it never
	// fails the build — only the summary reports it.
	ExpectedCategory string `json:"expected_category,omitempty"`
	GotCategory      string `json:"got_category,omitempty"`

	// Dangerous marks the one failure that matters: a question that needed the
	// corpus was sent to a model with no sources.
	Dangerous bool   `json:"dangerous"`
	Problem   string `json:"problem,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// needsCorpus groups the two intents that both retrieve. Confusing them is a
// missed optimisation; confusing either with "general" is a safety failure.
func needsCorpus(intent string) bool {
	return intent == string(router.IntentLegal) || intent == string(router.IntentDiscussion)
}

func TestRouting(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY is not set; routing evaluation needs a real model")
	}

	llm, err := ai.NewGeminiLLM(key)
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	r := router.New(llm, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dataset, err := LoadRoutingDataset()
	if err != nil {
		t.Fatalf("dataset: %v", err)
	}
	t.Logf("routing evaluation: %d cases against %s", len(dataset.Cases), llm.Model())

	var (
		results         []result
		correct         int
		dangerous       int
		byGroup         = map[string][2]int{} // group -> [correct, total]
		categoryCorrect int
		categoryTotal   int
	)

	for _, tc := range dataset.Cases {
		in := router.Input{Question: tc.Message, PriorSources: tc.PriorSources}
		for _, turn := range tc.History {
			role := ai.RoleUser
			if turn.Role == "assistant" {
				role = ai.RoleAssistant
			}
			in.History = append(in.History, router.Turn{Role: role, Text: turn.Text})
		}

		start := time.Now()
		decision := r.Route(context.Background(), in)
		elapsed := time.Since(start)

		got := string(decision.Intent)
		res := result{
			ID:        tc.ID,
			Message:   tc.Message,
			Expected:  tc.Expect,
			Got:       got,
			Rewrite:   decision.SearchText,
			Correct:   got == tc.Expect,
			ElapsedMS: elapsed.Milliseconds(),
		}

		if tc.ExpectCategory != "" {
			res.ExpectedCategory = tc.ExpectCategory
			res.GotCategory = string(decision.Category)
			categoryTotal++
			if res.GotCategory == res.ExpectedCategory {
				categoryCorrect++
			}
		}

		// The gate. Everything else is a note.
		if needsCorpus(tc.Expect) && !needsCorpus(got) {
			res.Dangerous = true
			res.Problem = "a question needing the corpus was routed to the general engine"
			dangerous++
		}

		if decision.Fallback {
			res.Problem = strings.TrimSpace(res.Problem + " (router fell back: " + decision.Reason + ")")
		}

		if len(tc.RewriteMustMention) > 0 {
			if !mentionsAny(decision.SearchText, tc.RewriteMustMention) {
				res.Correct = false
				res.Problem = fmt.Sprintf(
					"rewrite %q mentions none of %v, so retrieval sees a pronoun",
					decision.SearchText, tc.RewriteMustMention)
			}
		}
		if tc.ExpectPriorSources && !decision.UsePriorSources {
			res.Correct = false
			res.Problem = "did not carry the sources the question was about"
		}

		if res.Correct {
			correct++
		}

		group := strings.SplitN(tc.ID, "-", 2)[0]
		counts := byGroup[group]
		if res.Correct {
			counts[0]++
		}
		counts[1]++
		byGroup[group] = counts

		results = append(results, res)
	}

	report(t, results, byGroup, correct, dangerous)
	if categoryTotal > 0 {
		t.Logf("category accuracy %d/%d (%.0f%%) — never a hard gate, guard.AssertsLaw backstops every category",
			categoryCorrect, categoryTotal, float64(categoryCorrect)/float64(categoryTotal)*100)
	}

	// The gate, and the only hard assertion. Accuracy is reported and watched;
	// this one fails the build.
	if dangerous > 0 {
		t.Errorf("%d question(s) needing the corpus were routed to the general engine — "+
			"the chat engine's guard is the only thing standing between that and an "+
			"uncited answer about Ethiopian law", dangerous)
	}

	// A wide margin, deliberately: this is a regression alarm, not a target. The
	// per-group table is what a person reads.
	if accuracy := float64(correct) / float64(len(results)); accuracy < 0.85 {
		t.Errorf("routing accuracy %.0f%%, want at least 85%%", accuracy*100)
	}
}

func mentionsAny(text string, wanted []string) bool {
	lower := strings.ToLower(text)
	for _, w := range wanted {
		if strings.Contains(lower, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

func report(
	t *testing.T,
	results []result,
	byGroup map[string][2]int,
	correct, dangerous int,
) {
	t.Helper()

	t.Logf("accuracy %d/%d (%.0f%%), dangerous misroutes: %d",
		correct, len(results), float64(correct)/float64(len(results))*100, dangerous)

	groups := make([]string, 0, len(byGroup))
	for group := range byGroup {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		counts := byGroup[group]
		t.Logf("  %-12s %d/%d", group, counts[0], counts[1])
	}

	for _, res := range results {
		if res.Correct && !res.Dangerous {
			continue
		}
		t.Logf("  %s: expected %s, got %s — %s", res.ID, res.Expected, res.Got, res.Problem)
	}

	// Committed so the next change is compared against this run rather than
	// argued about.
	writeReport(t, "routing", results)
}

func writeReport(t *testing.T, name string, payload any) {
	t.Helper()

	dir := filepath.Join("results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("could not create results directory: %v", err)
		return
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Logf("could not encode report: %v", err)
		return
	}

	path := filepath.Join(dir, name+"_latest.json")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Logf("could not write report: %v", err)
		return
	}
	t.Logf("report written to %s", path)
}
