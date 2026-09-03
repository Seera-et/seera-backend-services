package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// entailmentMaxClaims mirrors llmRerankMaxCandidates: bounds one structured
// call so a listwise judgment does not degrade past what one response can
// reliably hold.
const entailmentMaxClaims = 20

// Claim pairs one cited source with the text of the answer it is meant to
// support. Extracted from the finished answer rather than tracked during
// streaming — verification only ever runs after the stream ends (see
// Service.Answer), so there is no reason to complicate MarkerFilter's
// stream-time state machine for something needed only once, afterward.
type Claim struct {
	Marker string
	Text   string
	Source string
}

// claimMarkerPattern finds marker occurrences in a finished answer. Unlike
// grounding.go's markerPattern (anchored, built to judge one bracketed span
// at flush time), this searches a whole answer for spans MarkerFilter has
// already validated and normalised to "[S1]" / "[S1, S2]" — so it does not
// need to tolerate decoration or lowercase the way the streaming filter does.
var claimMarkerPattern = regexp.MustCompile(`\[(S\d+(?:,\s*S\d+)*)\]`)

// extractClaims pairs each marker in a finished answer with the text
// immediately before it (since the previous marker, or the start of the
// answer) and the text of the source it names.
//
// A marker citing several sources for one statement ("[S1, S2]") produces one
// claim per source, since a citation can be independently unsupported even
// when another one covers the same sentence. A source cited more than once
// keeps only its first claim — bounding the batch the same way
// llmRerankMaxCandidates bounds Rerank's, and because the first occurrence is
// usually the sentence that actually states the rule; later repeats of the
// same marker are typically restating it, not adding a new claim to check.
func extractClaims(answer string, cited []Citation) []Claim {
	bySource := make(map[string]string, len(cited))
	for _, c := range cited {
		bySource[c.Marker] = c.Text
	}

	var claims []Claim
	seen := make(map[string]bool, len(cited))

	last := 0
	for _, m := range claimMarkerPattern.FindAllStringSubmatchIndex(answer, -1) {
		text := strings.TrimSpace(answer[last:m[0]])
		markers := answer[m[2]:m[3]]
		last = m[1]

		if text == "" {
			continue
		}
		for _, marker := range strings.Split(markers, ",") {
			marker = strings.TrimSpace(marker)
			if seen[marker] {
				continue
			}
			source, ok := bySource[marker]
			if !ok {
				// Cannot happen for an answer MarkerFilter has already
				// validated (every surviving marker names a real source),
				// but skipped rather than trusted blindly.
				continue
			}
			seen[marker] = true
			claims = append(claims, Claim{Marker: marker, Text: text, Source: source})
		}
	}
	return claims
}

// Verifier checks whether cited sources actually support the claims attached
// to them, returning one verdict per claim in the same order.
type Verifier interface {
	Verify(ctx context.Context, claims []Claim) ([]bool, error)
}

// LLMVerifier checks entailment with one structured call to whatever LLM is
// already wired in — the same reuse LLMReranker makes of the generation
// model, for the same reason: no new credential, and the model already
// reading these sources for generation is well placed to judge whether one
// of them actually supports a specific sentence.
//
// Experimental: see config.CitationEntailment's doc comment. Not wired by
// default anywhere; cmd/api/main.go only constructs one when the flag is on.
type LLMVerifier struct {
	llm ai.LLM
}

func NewLLMVerifier(llm ai.LLM) *LLMVerifier { return &LLMVerifier{llm: llm} }

const entailmentSystemPrompt = `You check whether a cited source actually supports the claim it is attached to.

For each numbered pair below, decide: does the source's text support the
claim, at least in substance? Exact wording is not required, but the source
must actually say or clearly imply the claim — being on the same general
topic is not enough, and neither is a related provision that does not itself
state what the claim says.

Return exactly one true/false verdict per pair, in the order given. true means
supported. Say nothing else.`

var entailmentSchema = &ai.Schema{
	Type: "object",
	Properties: map[string]*ai.Schema{
		"verdicts": {
			Type:        "array",
			Description: "One true/false verdict per pair, same order as listed. true means the source supports the claim.",
			Items:       &ai.Schema{Type: "boolean"},
		},
	},
	Required: []string{"verdicts"},
}

// Verify implements Verifier.
//
// Errors here are not special-cased for recovery, the same posture
// LLMReranker takes: the caller treats a failure as "skip the check for this
// answer," never as a reason to withhold or alter the answer itself — an
// experimental verification pass must not be able to make the product worse
// than not having it.
func (v *LLMVerifier) Verify(ctx context.Context, claims []Claim) ([]bool, error) {
	if len(claims) == 0 {
		return nil, nil
	}

	batch := claims
	if len(batch) > entailmentMaxClaims {
		batch = batch[:entailmentMaxClaims]
	}

	raw, _, err := v.llm.Complete(ctx, ai.Request{
		System:     entailmentSystemPrompt,
		Messages:   []ai.Message{{Role: ai.RoleUser, Content: buildEntailmentPrompt(batch)}},
		MaxTokens:  32 + 8*len(batch),
		JSONSchema: entailmentSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("llm verify: %w", err)
	}

	verdicts, err := parseEntailmentVerdicts(raw, len(batch))
	if err != nil {
		return nil, fmt.Errorf("llm verify: %w", err)
	}
	return verdicts, nil
}

func buildEntailmentPrompt(claims []Claim) string {
	var b strings.Builder
	for i, c := range claims {
		fmt.Fprintf(&b, "\n%d.\nClaim: %s\nSource: %s\n", i+1, c.Text, c.Source)
	}
	return b.String()
}

func parseEntailmentVerdicts(raw string, want int) ([]bool, error) {
	var payload struct {
		Verdicts []bool `json:"verdicts"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload); err != nil {
		return nil, fmt.Errorf("decode verdicts: %w", err)
	}
	if len(payload.Verdicts) != want {
		return nil, fmt.Errorf("got %d verdicts, want %d", len(payload.Verdicts), want)
	}
	return payload.Verdicts, nil
}
