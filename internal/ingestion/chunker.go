package ingestion

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

const (
	// MaxChunkTokens is the point at which an article is split.
	//
	// One chunk per article is the default: a chunk should be a semantically
	// complete provision, not an N-token window. Splitting only happens when an
	// article is long enough that a single chunk would dilute its own embedding.
	MaxChunkTokens = 1000

	// MinSplitTokens stops a trailing fragment from becoming its own chunk. A
	// 12-token chunk retrieves noisily and cites nothing useful.
	MinSplitTokens = 80
)

// Chunk turns one article into one or more chunks.
//
// Every chunk carries an EmbedText that begins with a context header —
// document title, chapter, article number and title. This is the single most
// important thing the chunker does. The raw text of a sub-article reads
// "(2) The period shall be thirty days."; embedded on its own it is
// unretrievable, because nothing in it says which period, under which law, in
// which country. The header is embedded and indexed; Text, the verbatim span,
// is what the user is shown and what a citation quotes.
func Chunk(doc legal.Document, article legal.Article) ([]legal.Chunk, error) {
	text := strings.TrimSpace(article.Text)
	if text == "" {
		return nil, fmt.Errorf("article %s has no text", article.Number)
	}

	header := ContextHeader(doc, article)
	segments := splitArticle(text)

	chunks := make([]legal.Chunk, 0, len(segments))
	for i, seg := range segments {
		chunks = append(chunks, legal.Chunk{
			ArticleID:  article.ID,
			VersionID:  article.VersionID,
			DocumentID: article.DocumentID,
			Language:   doc.Language,
			Text:       seg,
			EmbedText:  header + "\n\n" + seg,
			TokenCount: EstimateTokens(seg),
			Ordinal:    i,
		})
	}
	return chunks, nil
}

// ContextHeader builds the provenance prefix prepended to every chunk's
// embedded text.
func ContextHeader(doc legal.Document, article legal.Article) string {
	var b strings.Builder

	b.WriteString(doc.Title)
	if doc.IssuingAuthority != "" {
		b.WriteString(" (")
		b.WriteString(doc.IssuingAuthority)
		b.WriteString(")")
	}

	if len(article.SectionPath) > 0 {
		b.WriteString("\n")
		b.WriteString(strings.Join(article.SectionPath, " › "))
	} else if article.Chapter != "" {
		b.WriteString("\n")
		b.WriteString(article.Chapter)
	}

	b.WriteString("\nArticle ")
	b.WriteString(article.Number)
	if article.Title != "" {
		b.WriteString(" — ")
		b.WriteString(article.Title)
	}
	return b.String()
}

// splitArticle divides an over-long article at sub-article boundaries.
//
// Paragraphs are packed greedily and never split mid-paragraph unless a single
// paragraph is itself over the limit, because a sub-article cut in half is a
// legal provision cut in half.
func splitArticle(text string) []string {
	if EstimateTokens(text) <= MaxChunkTokens {
		return []string{text}
	}

	paragraphs := splitParagraphs(text)
	var (
		out     []string
		current strings.Builder
	)

	flush := func() {
		if s := strings.TrimSpace(current.String()); s != "" {
			out = append(out, s)
		}
		current.Reset()
	}

	for _, p := range paragraphs {
		if EstimateTokens(p) > MaxChunkTokens {
			// A single paragraph over the limit. Nothing to do but cut it, so
			// cut at sentence boundaries and accept the seam.
			flush()
			out = append(out, hardSplit(p)...)
			continue
		}
		if current.Len() > 0 && EstimateTokens(current.String()+p) > MaxChunkTokens {
			flush()
		}
		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(p)
	}
	flush()

	return mergeRunt(out)
}

// splitParagraphs breaks text at blank lines, and at sub-article markers such
// as "(2)" or "3." that start a line — the structure legal drafters actually
// use.
func splitParagraphs(text string) []string {
	lines := strings.Split(text, "\n")

	var (
		paragraphs []string
		current    []string
	)
	flush := func() {
		if len(current) > 0 {
			if s := strings.TrimSpace(strings.Join(current, "\n")); s != "" {
				paragraphs = append(paragraphs, s)
			}
			current = nil
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		if len(current) > 0 && startsSubArticle(trimmed) {
			flush()
		}
		current = append(current, trimmed)
	}
	flush()

	if len(paragraphs) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	return paragraphs
}

// startsSubArticle reports whether a line opens a numbered sub-provision:
// "(1) ...", "1) ...", "1. ...", or the Ge'ez equivalents.
func startsSubArticle(line string) bool {
	runes := []rune(line)
	i := 0
	if runes[i] == '(' {
		i++
	}

	start := i
	for i < len(runes) && (unicode.IsDigit(runes[i]) || isEthiopicNumeral(runes[i]) || isLetterOrdinal(runes, start, i)) {
		i++
	}
	if i == start || i >= len(runes) {
		return false
	}
	return runes[i] == ')' || runes[i] == '.'
}

// isLetterOrdinal allows a single trailing letter, as in "(1a)".
func isLetterOrdinal(runes []rune, start, i int) bool {
	return i > start && i-start == 1 && unicode.IsLetter(runes[i]) && i+1 < len(runes) &&
		(runes[i+1] == ')' || runes[i+1] == '.')
}

func isEthiopicNumeral(r rune) bool { return r >= 0x1369 && r <= 0x137C }

// hardSplit cuts an oversized paragraph at sentence boundaries.
func hardSplit(p string) []string {
	sentences := splitSentences(p)

	var (
		out     []string
		current strings.Builder
	)
	for _, s := range sentences {
		if current.Len() > 0 && EstimateTokens(current.String()+s) > MaxChunkTokens {
			out = append(out, strings.TrimSpace(current.String()))
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString(" ")
		}
		current.WriteString(s)
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// splitSentences breaks on "." and on the Ethiopic full stop "።".
func splitSentences(p string) []string {
	var (
		out     []string
		current strings.Builder
	)
	for _, r := range p {
		current.WriteRune(r)
		if r == '.' || r == '።' {
			if s := strings.TrimSpace(current.String()); s != "" {
				out = append(out, s)
			}
			current.Reset()
		}
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{p}
	}
	return out
}

// mergeRunt folds a too-small trailing chunk into its predecessor.
func mergeRunt(chunks []string) []string {
	if len(chunks) < 2 {
		return chunks
	}
	last := len(chunks) - 1
	if EstimateTokens(chunks[last]) < MinSplitTokens {
		chunks[last-1] += "\n\n" + chunks[last]
		chunks = chunks[:last]
	}
	return chunks
}

// EstimateTokens approximates the token count of a string.
//
// It is an estimate, used only to decide where to split; nothing bills against
// it. Latin script runs about four characters per token. Ethiopic does far
// worse under byte-pair encodings trained mostly on Latin text — roughly one
// token per character — so counting it at parity would badly under-split
// Amharic articles.
func EstimateTokens(s string) int {
	var latin, ethiopic, other int
	for _, r := range s {
		switch {
		case r >= 0x1200 && r <= 0x137F: // Ethiopic block
			ethiopic++
		case r < unicode.MaxASCII:
			latin++
		default:
			other++
		}
	}
	tokens := (latin+3)/4 + ethiopic + (other+1)/2
	if tokens == 0 && len(s) > 0 {
		return 1
	}
	return tokens
}
