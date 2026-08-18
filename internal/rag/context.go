package rag

import (
	"fmt"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/search"
)

// DefaultContextTokenBudget caps the sources block.
//
// Generous, because the whole architecture rests on the model seeing the actual
// provisions. But bounded, because an over-long context degrades grounding —
// the relevant article competes with everything else in the window.
const DefaultContextTokenBudget = 12000

// SourceRef ties a marker the model can write to a chunk the database knows.
//
// Sources are numbered [S1], [S2] rather than labelled with chunk UUIDs. This is
// the one place the design departs from "cite the chunk_id", and deliberately:
// a UUID is 36 characters the model has to reproduce exactly, and a single
// transposed character turns a correct citation into an unresolvable one. A
// small integer cannot be mistyped into a *different valid* source, and any
// marker outside the range is provably invalid. The mapping from marker to
// chunk_id lives here, server-side, so the citation still resolves to an
// internal database record and is never model-authored.
type SourceRef struct {
	Marker string // "S1"
	Result search.Result
}

// Context is the sources block plus the mapping needed to validate what comes
// back.
type Context struct {
	Block   string
	Sources []SourceRef

	// Dropped counts results that did not fit the budget, so the log can show
	// when the context is the constraint.
	Dropped int
}

// BuildContext renders retrieved chunks into the sources block.
//
// Results must arrive best-first: the budget is spent from the front, so what
// gets dropped is what the reranker liked least.
func BuildContext(results []search.Result, tokenBudget int) Context {
	if tokenBudget <= 0 {
		tokenBudget = DefaultContextTokenBudget
	}

	var (
		b       strings.Builder
		sources []SourceRef
		spent   int
	)

	for i, r := range results {
		marker := fmt.Sprintf("S%d", i+1)
		rendered := renderSource(marker, r)

		cost := estimateTokens(rendered)
		if spent+cost > tokenBudget && len(sources) > 0 {
			// Stop at the first source that does not fit rather than skipping it
			// and taking a later one: the order is the reranker's judgement, and
			// reordering it here would silently override that.
			return Context{
				Block:   b.String(),
				Sources: sources,
				Dropped: len(results) - len(sources),
			}
		}

		b.WriteString(rendered)
		spent += cost
		sources = append(sources, SourceRef{Marker: marker, Result: r})
	}

	return Context{Block: b.String(), Sources: sources}
}

// renderSource writes one numbered source with its full provenance.
//
// Every field here has a job. The document title and article number let the
// model attribute a claim; the version label and effective date let it say which
// version of the law it relied on; the language lets it answer in kind.
func renderSource(marker string, r search.Result) string {
	var b strings.Builder

	b.WriteString("[")
	b.WriteString(marker)
	b.WriteString("] ")
	b.WriteString(r.DocumentTitle)

	if r.ArticleNo != "" {
		b.WriteString(", Article ")
		b.WriteString(r.ArticleNo)
	}
	if r.ArticleTitle != "" {
		b.WriteString(" — ")
		b.WriteString(r.ArticleTitle)
	}
	b.WriteString("\n")

	if len(r.SectionPath) > 0 {
		b.WriteString("Location: ")
		b.WriteString(strings.Join(r.SectionPath, " > "))
		b.WriteString("\n")
	}
	if r.IssuingAuthority != "" {
		b.WriteString("Issued by: ")
		b.WriteString(r.IssuingAuthority)
		b.WriteString("\n")
	}
	if r.EffectiveFrom != nil {
		b.WriteString("In force from: ")
		b.WriteString(r.EffectiveFrom.Format("2006-01-02"))
		b.WriteString("\n")
	}
	b.WriteString("Language: ")
	b.WriteString(string(r.Language))
	b.WriteString("\n\n")

	b.WriteString(r.Text)
	b.WriteString("\n\n---\n\n")

	return b.String()
}

// estimateTokens approximates a token count for budgeting only.
//
// Latin runs about four characters per token; Ethiopic does far worse under
// encodings trained mostly on Latin text. Mirrors the ingestion chunker's
// estimator so the two agree about what "1000 tokens" means.
func estimateTokens(s string) int {
	var latin, ethiopic, other int
	for _, r := range s {
		switch {
		case r >= 0x1200 && r <= 0x137F:
			ethiopic++
		case r < 0x80:
			latin++
		default:
			other++
		}
	}
	return (latin+3)/4 + ethiopic + (other+1)/2
}
