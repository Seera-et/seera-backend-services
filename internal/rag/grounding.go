package rag

import (
	"regexp"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// SystemPrompt is the generation contract.
//
// It contains no legal text and never will. Every provision the model sees
// arrives from retrieval in the sources block; if a rule is not in the corpus,
// the correct output is an admission, not a recollection. Hardcoding any
// Ethiopian legal content here would also make the law un-updatable without a
// deploy.
const SystemPrompt = `You answer questions about Ethiopian law using only the sources provided to you.

Rules:

1. Use only the numbered sources below. Do not use anything you know about
   Ethiopian law or any other law from outside them. If the sources do not
   contain the answer, say so plainly and stop. Start that reply with
   NOT_IN_SOURCES on the first line, alone, before your explanation — it tells
   the system not to present the answer as though the law had been found. Use it
   whenever the honest answer is "these provisions do not settle this", even if
   they touch the subject and you can say something about them.
2. Cite every legal statement with the marker of the source it comes from, like
   [S1] or [S2, S3]. Put the marker immediately after the statement it supports.
   A sentence that states what the law says and carries no marker is an error.
   Put nothing else inside the brackets — no article numbers, no titles, no
   comments. Write "Article 265(1) requires registration [S1]", never
   "[S1, Art. 265(1)]". The article number is already attached to the source and
   is shown to the reader from the record, not from what you type.
3. Quote or closely paraphrase the provision. Do not smooth over conditions,
   exceptions, time limits, or thresholds — those are usually the answer.
4. Then explain it. State what the provision says, and follow with what it means
   in plain language: define the terms it uses, spell out what follows from it,
   take its conditions in order and say what each one requires. Legal text is
   written for lawyers; the explanation is most of the value here.
5. Reason only from the sources. Working through what a provision implies, how
   two of them fit together, or which one governs a situation is exactly what
   you should do — inventing a rule that none of them contains is not. If a step
   in your reasoning needs something the sources do not say, say that it is
   missing instead of filling it in.
6. Distinguish what the law says from how it is applied or interpreted. State
   the provision first; mark anything beyond it as interpretation.
7. Answer in the language of the question. If the sources are in a different
   language from the question, translate faithfully and say that you have.
8. If sources conflict, or a provision may have been amended or repealed, say so
   and name the version and date each source carries.
9. Do not give advice about what the person should do in their own situation, and
   do not predict how a court would decide. Describe the law and let them draw
   conclusions.
10. Be direct. Lead with the answer, then the supporting provisions and the
    explanation. No preamble.
11. Earlier turns of the conversation, when present, tell you what is being asked
    — a pronoun, "that article", "what about…". They are not sources. Never
    repeat a legal statement from an earlier turn unless a source below supports
    it now, and cite that source when you do.

You are not a lawyer and this is not legal advice.`

// AbstainMessage is returned when retrieval found nothing usable.
//
// It names what was searched and suggests a narrowing, and does not apologise
// and then guess. An honest "not found" is a correct answer; a plausible
// invention is the failure this whole system is built to avoid.
const AbstainMessage = `I could not find anything in the indexed Ethiopian legal sources that answers this reliably, so I am not going to guess.

What might help:
- naming the specific law, code, or proclamation you have in mind
- citing an article number directly, if you know one
- asking about a narrower point

The corpus does not yet cover all Ethiopian law, so an answer being absent here does not mean the law is silent.`

// markerPattern matches a citation marker: the prompt asks for [S1], [S2,S3],
// [S1, S12].
//
// It also tolerates a marker the model has decorated — [S1, Art. 265(1)] — by
// capturing the source numbers and treating the rest as noise. Models add that
// article reference readily, and refusing to recognise it was worse than
// pointless: the citation was dropped, the answer was reported as ungrounded,
// and the reader was warned about an answer that was in fact cited throughout.
//
// The decoration is discarded rather than displayed. An article number the model
// typed is a model-authored citation string; the resolved citation carries the
// article number the database holds, and that is the one the reader is shown.
var markerPattern = regexp.MustCompile(`(?i)^\[\s*(s\d+(?:\s*,\s*s\d+)*)([^\]]*)\]$`)

// Citation is a resolved reference attached to an answer.
//
// The frontend renders from these, never by parsing the answer text — that is
// what makes a citation a database record rather than a string the model wrote.
type Citation struct {
	Marker string `json:"marker"`

	ChunkID    legal.ChunkID    `json:"chunk_id"`
	ArticleID  legal.ArticleID  `json:"article_id"`
	VersionID  legal.VersionID  `json:"version_id"`
	DocumentID legal.DocumentID `json:"document_id"`

	DocumentTitle    string   `json:"document_title"`
	IssuingAuthority string   `json:"issuing_authority,omitempty"`
	ArticleNo        string   `json:"article_no"`
	ArticleTitle     string   `json:"article_title,omitempty"`
	SectionPath      []string `json:"section_path,omitempty"`
	Language         string   `json:"language"`
	EffectiveFrom    string   `json:"effective_from,omitempty"`

	Text string `json:"text"`

	// StructureConfidence is surfaced because a chunk recovered by window
	// fallback rather than article detection is a weaker citation, and the UI
	// and the evaluation harness both deserve to know.
	StructureConfidence string `json:"structure_confidence,omitempty"`
}

// citationFrom converts a source reference into its citation.
func citationFrom(ref SourceRef) Citation {
	r := ref.Result

	c := Citation{
		Marker:              ref.Marker,
		ChunkID:             r.ChunkID,
		ArticleID:           r.ArticleID,
		VersionID:           r.VersionID,
		DocumentID:          r.DocumentID,
		DocumentTitle:       r.DocumentTitle,
		IssuingAuthority:    r.IssuingAuthority,
		ArticleNo:           r.ArticleNo,
		ArticleTitle:        r.ArticleTitle,
		SectionPath:         r.SectionPath,
		Language:            string(r.Language),
		Text:                r.Text,
		StructureConfidence: string(r.StructureConfidence),
	}
	if r.EffectiveFrom != nil {
		c.EffectiveFrom = r.EffectiveFrom.Format("2006-01-02")
	}
	return c
}

// MarkerFilter sanitises citation markers in a token stream.
//
// Grounding cannot be checked only after generation, because by then the text is
// already on the user's screen. So markers are validated as they stream: text is
// released immediately, except that an opening bracket holds the stream until the
// marker closes, at which point it is either passed through (it names real
// sources) or dropped (it does not). A marker the model invented never reaches
// the client.
//
// Not safe for concurrent use; one filter belongs to one stream.
type MarkerFilter struct {
	valid map[string]bool
	buf   strings.Builder

	// used records which markers survived, in first-seen order, so the response
	// carries exactly the citations the answer actually makes.
	used []string
	seen map[string]bool

	// invalid counts markers that named a source that was not in the context.
	invalid int
}

// maxMarkerLength bounds how long the filter will hold text waiting for a
// closing bracket. Longer than any real marker; a bracket that never closes is
// released as ordinary text rather than swallowing the answer.
const maxMarkerLength = 64

// NewMarkerFilter builds a filter that accepts only the given sources' markers.
func NewMarkerFilter(sources []SourceRef) *MarkerFilter {
	valid := make(map[string]bool, len(sources))
	for _, s := range sources {
		valid[s.Marker] = true
	}
	return &MarkerFilter{valid: valid, seen: map[string]bool{}}
}

// Write feeds a chunk of generated text through the filter and returns the text
// that is safe to send on.
func (f *MarkerFilter) Write(text string) string {
	var out strings.Builder

	for _, r := range text {
		if f.buf.Len() > 0 {
			f.buf.WriteRune(r)
			if r == ']' {
				out.WriteString(f.flush())
			} else if f.buf.Len() > maxMarkerLength {
				// Not a marker after all.
				out.WriteString(f.release())
			}
			continue
		}
		if r == '[' {
			f.buf.WriteRune(r)
			continue
		}
		out.WriteRune(r)
	}

	return out.String()
}

// Close releases anything still buffered at the end of the stream.
func (f *MarkerFilter) Close() string { return f.release() }

// flush decides the fate of a completed bracketed span.
func (f *MarkerFilter) flush() string {
	span := f.buf.String()
	f.buf.Reset()

	groups := markerPattern.FindStringSubmatch(span)
	if groups == nil {
		// A bracketed span that is not a citation — keep it verbatim.
		return span
	}

	var kept []string
	for _, marker := range strings.Split(groups[1], ",") {
		// Upper-cased because the source numbers are S1..Sn by construction, and
		// a model that writes [s1] means the same source.
		marker = strings.ToUpper(strings.TrimSpace(marker))
		if !f.valid[marker] {
			f.invalid++
			continue
		}
		kept = append(kept, marker)
		if !f.seen[marker] {
			f.seen[marker] = true
			f.used = append(f.used, marker)
		}
	}

	if len(kept) == 0 {
		// Every marker in the span was invented. Drop the whole marker rather
		// than leaving an empty bracket in the answer.
		return ""
	}
	return "[" + strings.Join(kept, ", ") + "]"
}

// release empties the buffer without interpreting it.
func (f *MarkerFilter) release() string {
	if f.buf.Len() == 0 {
		return ""
	}
	span := f.buf.String()
	f.buf.Reset()
	return span
}

// Used returns the markers the answer cited, in first-seen order.
func (f *MarkerFilter) Used() []string { return f.used }

// Invalid returns how many markers named a source that was not in the context.
// Non-zero is a bug worth logging: the model was given numbered sources and
// still produced a number outside the range.
func (f *MarkerFilter) Invalid() int { return f.invalid }

// looksSubstantive reports whether an answer makes claims that need citations.
//
// Used to catch the failure mode where a fluent answer arrives with no markers at
// all. Short refusals and clarifying questions legitimately carry none, so
// length is the discriminator rather than content.
func looksSubstantive(answer string) bool {
	return len([]rune(strings.TrimSpace(answer))) > 240
}
