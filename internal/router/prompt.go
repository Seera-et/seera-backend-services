package router

import (
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// systemPrompt is the routing contract.
//
// It contains no legal text and asks for none. The model is sorting a message
// and rewriting a sentence, not answering anything — and the instructions say so
// explicitly, because a model handed a legal question will otherwise try to
// answer it in the field meant for a rewrite.
//
// The bias is stated rather than implied: when in doubt, choose the path that
// searches the corpus. An unnecessary search costs a second; an unnecessary
// guess costs the thing this product exists to protect.
const systemPrompt = `You route messages arriving at Seera, an assistant that answers questions about
Ethiopian law from a corpus of indexed legal documents.

You do not answer the message. You decide who should, and prepare the search.

Choose one intent:

- "legal": the message asks about Ethiopian law, or about a situation whose
  answer depends on it. Anything about rights, obligations, procedures,
  registration, licensing, penalties, deadlines, contracts, employment, tax, or
  what a specific article, code or proclamation says.
- "discussion": the message is about the sources already cited earlier in this
  conversation — "explain the second one", "what does that article mean in
  practice", "is that still in force". Use this only when earlier sources are
  listed below.
- "general": everything else a legal assistant should still answer. Greetings,
  thanks, questions about what Seera is or can do, requests to rephrase, and
  general concepts that do not depend on Ethiopian law in particular ("what is a
  limited company, in general terms").

When the message could be either, choose "legal". Searching the corpus and
finding nothing is a small cost. Answering a legal question from memory is not
acceptable at all.

Choose one depth, which controls how much the answer explains:

- "quote": the reader wants the provision itself.
- "explain": the reader wants to understand what it means. This is the usual case.
- "analyse": the reader wants several provisions applied to a situation together.

Then write "search_text": the message as a question that can be understood on its
own, for a search engine that cannot see this conversation. Resolve pronouns and
references ("it", "that article", "what about…") using the earlier turns. Keep
the language of the message. If the message already stands alone, repeat it
unchanged. Add no facts, article numbers or conditions that are not already in
the conversation. For "general", leave it empty.

Set "use_prior_sources" to true when answering will need the sources already
cited in this conversation.

"reason" is at most one short clause, for a log.`

// decisionSchema constrains the response. The provider enforces the shape, and
// parseDecision still validates the values.
var decisionSchema = &ai.Schema{
	Type: "object",
	Properties: map[string]*ai.Schema{
		"intent": {
			Type: "string",
			Enum: []string{
				string(IntentLegal),
				string(IntentDiscussion),
				string(IntentGeneral),
			},
			Description: "Which engine should answer.",
		},
		"depth": {
			Type: "string",
			Enum: []string{
				string(DepthQuote),
				string(DepthExplain),
				string(DepthAnalyse),
			},
			Description: "How much the answer should explain.",
		},
		"search_text": {
			Type:        "string",
			Description: "The message as a standalone question for retrieval. Empty for general.",
		},
		"use_prior_sources": {
			Type:        "boolean",
			Description: "Whether the sources already cited are needed to answer.",
		},
		"reason": {
			Type:        "string",
			Description: "One short clause explaining the choice.",
		},
	},
	Required: []string{"intent", "depth", "search_text", "use_prior_sources"},
}

// buildPrompt renders one routing request.
func buildPrompt(in Input) string {
	var b strings.Builder

	if len(in.History) > 0 {
		b.WriteString("Conversation so far:\n\n")
		for _, turn := range trimHistory(in.History) {
			if turn.Role == ai.RoleAssistant {
				b.WriteString("Assistant: ")
			} else {
				b.WriteString("User: ")
			}
			b.WriteString(turn.Text)
			b.WriteString("\n\n")
		}
	}

	if len(in.PriorSources) > 0 {
		b.WriteString("Sources already cited in this conversation:\n")
		for i, source := range in.PriorSources {
			if i >= maxPriorSources {
				break
			}
			b.WriteString("- ")
			b.WriteString(source)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("Message to route: ")
	b.WriteString(strings.TrimSpace(in.Question))

	return b.String()
}

// trimHistory bounds what is quoted back to the model: the most recent turns,
// each cut to length.
func trimHistory(history []Turn) []Turn {
	if len(history) > maxHistoryTurns {
		history = history[len(history)-maxHistoryTurns:]
	}

	out := make([]Turn, 0, len(history))
	for _, turn := range history {
		text := strings.TrimSpace(turn.Text)
		if text == "" {
			continue
		}
		turn.Text = truncate(text, maxTurnRunes)
		out = append(out, turn)
	}
	return out
}
