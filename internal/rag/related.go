package rag

import "fmt"

// Suggestion is a follow-up question offered alongside an answer.
type Suggestion struct {
	Question string `json:"question"`
}

// maxSuggestions keeps the offer to a glance.
const maxSuggestions = 3

// relatedQuestions builds follow-ups from the citations an answer actually used.
//
// Constructed from stored metadata — a document title, an article number — and
// never generated. A model writing these would eventually offer "What is the
// penalty under Article 45?" for an article that says nothing about penalties,
// and an invented question is a claim that such a provision exists.
//
// They are questions, not statements, and every one of them goes back through
// retrieval like any other.
func relatedQuestions(cited []Citation) []Suggestion {
	if len(cited) == 0 {
		return nil
	}

	var (
		out       []Suggestion
		seenDocs  = map[string]bool{}
		documents []string
	)

	add := func(question string) {
		if len(out) >= maxSuggestions {
			return
		}
		for _, existing := range out {
			if existing.Question == question {
				return
			}
		}
		out = append(out, Suggestion{Question: question})
	}

	for _, c := range cited {
		if c.DocumentTitle != "" && !seenDocs[c.DocumentTitle] {
			seenDocs[c.DocumentTitle] = true
			documents = append(documents, c.DocumentTitle)
		}
	}

	// The provision the answer leaned on first, explained further.
	if first := cited[0]; first.ArticleNo != "" && first.DocumentTitle != "" {
		add(fmt.Sprintf("Explain Article %s of the %s in plain language",
			first.ArticleNo, first.DocumentTitle))
	}

	// How the answer's sources fit together, when it used more than one.
	if len(documents) > 1 {
		add(fmt.Sprintf("How do the %s and the %s apply together here?",
			documents[0], documents[1]))
	} else if len(cited) > 1 && cited[1].ArticleNo != "" {
		add(fmt.Sprintf("What does Article %s add to this?", cited[1].ArticleNo))
	}

	// Which version applied — the question a reader should ask more often than
	// they do.
	if first := cited[0]; first.DocumentTitle != "" {
		add(fmt.Sprintf("Has this part of the %s been amended?", first.DocumentTitle))
	}

	return out
}
