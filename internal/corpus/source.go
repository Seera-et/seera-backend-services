// Package corpus bridges acquired documents into the indexed corpus.
//
// The crawler stops at a validated PDF and a metadata record; workers/extraction
// turns that PDF into structured JSON; internal/ingestion turns that JSON into
// rows and vectors. Nothing connected the three, so adding documents meant
// retyping each one's metadata into a curl command — metadata the crawler had
// already scraped — with no record of which documents had made it in.
//
// This package is that connection, and the judgement that goes with it: which
// documents are fit to publish without a human reading the parse first.
package corpus

import (
	"github.com/Seera-et/seera-backend-services/internal/crawler"
)

// Source is the acquired corpus on disk.
//
// An interface, narrowly, so that only one file in this package imports
// internal/crawler. That package carries the whole acquisition stack — an HTTP
// client, discovery, the PDF resolver — and this one runs in a binary that must
// never make a request to justice.gov.et. Keeping the edge in a single adapter
// means the rest of the package tests against a fake and cannot grow a
// dependency on the network by accident.
type Source interface {
	// Records are every acquired document, in a deterministic order.
	Records() ([]crawler.Record, error)

	// StructuredPath is where workers/extraction wrote this document's JSON.
	// The file may not exist: extraction is a separate, manual step.
	StructuredPath(docID string) string
}
