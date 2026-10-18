package ingestion

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// PublishPolicy says when a freshly ingested version should go live.
//
// A policy rather than a bool because the interesting case cannot be decided by
// the caller: "publish unless this document already has something published"
// depends on database state, and checking it before Ingest is a race.
// PublishVersion supersedes whatever is currently published before publishing,
// so two ingests that both found nothing published do not collide — the second
// silently demotes the first. The decision therefore has to happen inside the
// publishing transaction, which means Ingest has to be told the rule rather
// than the answer.
type PublishPolicy string

const (
	// PublishNever leaves the version as a draft. Invisible to retrieval, which
	// is the right default for a parse nobody has looked at.
	PublishNever PublishPolicy = "never"

	// PublishAlways publishes unconditionally, superseding any current version.
	PublishAlways PublishPolicy = "always"

	// PublishIfNoneLive publishes only when the document has no published
	// version yet. The safe automatic choice: it can introduce a law, but it
	// can never demote an edition someone already vetted.
	PublishIfNoneLive PublishPolicy = "if-none-live"
)

// Valid reports whether p is a policy Ingest understands.
func (p PublishPolicy) Valid() bool {
	switch p {
	case PublishNever, PublishAlways, PublishIfNoneLive:
		return true
	default:
		return false
	}
}

// ErrInvalidRequest wraps every problem Validate finds, so callers classify it
// by type rather than by matching on the text of the message.
var ErrInvalidRequest = errors.New("invalid request")

// Validate checks everything about a request that can be known without touching
// the database or the embedding provider.
//
// This lives on the request rather than in the HTTP handler because it is not
// an HTTP concern. It used to be, and the consequence was that any caller which
// was not the handler — the ingest CLI, a test — silently got none of it. An
// empty title is the sharpest example: Ingest would accept it, the first such
// document would insert, and the second would *match* it through
// FindDocumentByTitle(ctx, "", lang) and attach its version to the same
// document row. Two unrelated laws merged into one identity, with the second
// publication superseding the first, and every chunk embedded under an empty
// context header.
//
// Every problem is collected rather than returned one at a time: an operator
// fixing an extraction wants the whole list, not six round trips.
func (r Request) Validate() error {
	var problems []error

	if strings.TrimSpace(r.Document.Title) == "" {
		problems = append(problems, errors.New("document.title is required"))
	}
	if strings.TrimSpace(r.Version.Label) == "" {
		problems = append(problems, errors.New("version.label is required"))
	}

	switch r.Document.DocType {
	case legal.DocTypeProclamation, legal.DocTypeRegulation, legal.DocTypeDirective,
		legal.DocTypeCode, legal.DocTypeOther:
	case "":
		problems = append(problems, errors.New("document.doc_type is required"))
	default:
		problems = append(problems, fmt.Errorf(
			"document.doc_type %q is not one of proclamation, regulation, directive, code, other",
			r.Document.DocType))
	}

	if !r.Document.Language.Valid() {
		problems = append(problems, fmt.Errorf(
			"document.language %q is not one of en, am", r.Document.Language))
	}

	if !r.PublishPolicy.Valid() {
		problems = append(problems, fmt.Errorf(
			"publish policy %q is not one of never, always, if-none-live", r.PublishPolicy))
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, errors.Join(problems...))
	}
	return nil
}
