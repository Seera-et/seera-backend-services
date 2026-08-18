package ai

import "context"

// Embedder turns text into vectors.
//
// Model, Version, and Dimensions are part of the interface because they are
// part of the data: every stored vector records which model produced it, and
// each embedding space lives in its own table sized to its own dimension count.
// A model change is a new table plus a backfill, never mixed vectors in one
// index — cosine distance between vectors from two different models is
// meaningless, and the failure is silent.
type Embedder interface {
	// Embed returns one vector per input, in input order.
	//
	// Implementations batch internally and retry transient failures. A partial
	// result is never returned: either every input has a vector, or Embed
	// fails.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// EmbedQuery embeds a search query rather than a document.
	//
	// The distinction is not cosmetic. Some providers — Gemini among them —
	// embed a passage and a question about that passage into deliberately
	// different spaces, and passing a query through the document path costs
	// retrieval accuracy for nothing. Providers without the distinction
	// implement this as Embed of one text.
	EmbedQuery(ctx context.Context, text string) ([]float32, error)

	// Model is the provider's model identifier, e.g. "text-embedding-3-large".
	Model() string

	// Version distinguishes configurations of the same model that produce
	// incompatible vectors — dimension reductions, revised preprocessing.
	Version() string

	// Dimensions is the length of every vector Embed returns. It must match the
	// vector(N) column of the table this embedder writes to.
	Dimensions() int
}
