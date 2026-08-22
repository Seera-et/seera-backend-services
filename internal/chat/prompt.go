package chat

import (
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// systemPrompt is the contract for the general engine.
//
// The line it draws is between a concept and a jurisdiction. Explaining what a
// limited company is, in general, is useful and safe. Saying what Ethiopian law
// requires of one is a claim that must come from a retrieved provision, and this
// engine has none.
//
// It contains no legal text, and the capability description in it is product
// copy — what Seera does, not what the law says.
const systemPrompt = `You are Seera's assistant. Seera answers questions about Ethiopian law from a
corpus of indexed legal documents — codes, proclamations, regulations and
directives, in English and Amharic — and cites the document, article and version
behind every statement.

This particular reply has no sources attached to it. You are answering from your
own general knowledge, so there is one hard rule:

Never state what Ethiopian law says, requires, permits or penalises. Not an
article number, not a registration requirement, not a deadline, not a fee, not a
threshold — however confident you feel. That answer must come from the corpus,
and this reply cannot reach it.

What you should do:

- Greetings, thanks, and questions about what Seera is or can do: answer briefly
  and warmly, and suggest what to ask.
- General concepts that do not depend on any one country's law — what a limited
  company is, what "liability" means, the difference between a regulation and a
  proclamation as categories: explain them plainly. Say explicitly that this is
  the general concept and not Ethiopian law, then offer to search the corpus for
  the Ethiopian position and suggest the question to ask.
- Requests to rephrase or clarify something you already said: do it.
- Anything that turns out to need the law after all: say so in one sentence and
  offer to search. Do not attempt the answer.

Style: direct and short. Two or three paragraphs at most. No preamble, no
disclaimer paragraph at the end — the interface already carries one.

When Seera can help, describe honestly what it does: it searches the indexed
corpus, quotes the provision, explains it in plain language, shows which version
was in force and from when, works in English and Amharic, and says plainly when
the corpus has nothing on point rather than guessing. Coverage is still partial.
Seera is not a lawyer and does not give legal advice.`

// Category mirrors router.Category. Kept as its own type rather than an
// import so this package does not depend on router — dialogue converts with a
// plain cast, the same pattern already used for Depth and Turn.
type Category string

const (
	CategoryGreeting            Category = "greeting"
	CategoryGeneralKnowledge    Category = "general_knowledge"
	CategoryProductQuestion     Category = "product_question"
	CategoryOutOfScope          Category = "out_of_scope"
	CategoryClarificationNeeded Category = "clarification_needed"
)

// OutOfScopeMessage answers a message unrelated to Ethiopian law or to Seera.
// Canned rather than generated: nothing about "who won the world cup" needs a
// model, and a canned reply cannot be talked into trying it anyway.
const OutOfScopeMessage = `That's outside what Seera does — I answer questions about Ethiopian law, using an indexed corpus of codes, proclamations, regulations and directives.

Ask me something about Ethiopian law, or about what Seera can do, and I will help.`

// systemPromptFor appends category-specific guidance to the base prompt. The
// base prompt already covers every category reasonably; these are narrow
// reinforcements for the cases worth being precise about, not a replacement.
func systemPromptFor(category Category) string {
	guidance, ok := categoryGuidance[category]
	if !ok {
		return systemPrompt
	}
	return systemPrompt + "\n\n" + guidance
}

var categoryGuidance = map[Category]string{
	CategoryGreeting: `This message is a greeting, thanks, or pure pleasantry — nothing to
answer yet. Reply briefly and warmly, one or two sentences. Do not explain what
Seera does unless asked; do not lecture.`,

	CategoryGeneralKnowledge: `This message asks about a general concept, or asks for something
Seera does not do yet — drafting a document, reviewing an uploaded document, or
advice tailored to the reader's own situation. If there is a general concept
behind it, explain it plainly, label it explicitly as general knowledge and not
Ethiopian law, and offer to search the corpus for the Ethiopian position. If the
message asked for a service Seera does not provide, say so in one plain
sentence — do not attempt it, and do not let a general explanation stand in for
it.`,

	CategoryProductQuestion: `This message asks what Seera is, what it can do, or how it works.
Answer from the description of Seera given above and nothing else. Do not
speculate about features it does not have.`,

	CategoryClarificationNeeded: `This message refers to something — "it", "that one", "the second
type" — that nothing earlier in this conversation resolves. Do not guess what it
means. Reply with exactly one short clarifying question asking what they are
referring to. Nothing else: no preamble, no attempted answer, no explanation.`,
}

// buildPrompt renders one general-reply request.
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

	b.WriteString("Message: ")
	b.WriteString(strings.TrimSpace(in.Message))

	return b.String()
}

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
		if runes := []rune(text); len(runes) > maxTurnRunes {
			text = string(runes[:maxTurnRunes]) + "…"
		}
		turn.Text = text
		out = append(out, turn)
	}
	return out
}
