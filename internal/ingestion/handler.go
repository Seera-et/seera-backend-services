package ingestion

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Handler exposes ingestion over HTTP.
//
// This file is the one addition to the agreed package layout: the ingest
// endpoint cannot live in internal/legal, because internal/ingestion already
// imports internal/legal and the reverse edge would be an import cycle.
//
// It writes its own JSON rather than reusing the server package's helpers,
// deliberately: this is an admin endpoint, and unlike the public API it returns
// full validation detail. An operator fixing a bad extraction needs to know
// exactly which article failed and why.
type Handler struct {
	service *Service
	log     *slog.Logger
}

func NewHandler(service *Service, log *slog.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// ingestRequest is the wire shape of POST /api/v1/admin/documents/ingest.
type ingestRequest struct {
	Document struct {
		Title            string `json:"title"`
		DocType          string `json:"doc_type"`
		LegalDomain      string `json:"legal_domain"`
		IssuingAuthority string `json:"issuing_authority"`
		PublicationDate  string `json:"publication_date"` // YYYY-MM-DD
		EffectiveDate    string `json:"effective_date"`   // YYYY-MM-DD
		Language         string `json:"language"`
		SourceURL        string `json:"source_url"`
		StorageKey       string `json:"storage_key"`
	} `json:"document"`

	Version struct {
		Label         string `json:"label"`
		EffectiveFrom string `json:"effective_from"` // YYYY-MM-DD
		RepealedAt    string `json:"repealed_at"`    // YYYY-MM-DD
	} `json:"version"`

	// Publish defaults to false: a document is not visible to retrieval until
	// someone has looked at how it parsed.
	Publish bool `json:"publish"`

	// SourceChecksum is the sha256 of the file the extraction came from.
	// Optional over HTTP — a hand-assembled request may not have one — but
	// supplying it makes a repeat request a cheap no-op instead of a document's
	// worth of embedding calls followed by a conflict.
	SourceChecksum string `json:"source_checksum"`

	// Replace deletes the version these bytes previously produced. Destructive,
	// so it is never the default.
	Replace bool `json:"replace"`

	Extraction Extraction `json:"extraction"`
}

const dateLayout = "2006-01-02"

// ServeHTTP handles one ingestion.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req ingestRequest
	if err := dec.Decode(&req); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			h.fail(w, r, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("Extraction exceeds the %d byte limit. Raise MAX_INGEST_BYTES.", maxBytes.Limit))
			return
		}
		h.fail(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	ingestReq, err := req.toServiceRequest()
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	result, err := h.service.Ingest(r.Context(), ingestReq)
	switch {
	case err == nil:
	case errors.Is(err, legal.ErrDuplicateDocument):
		// Not a server fault: the operator is re-ingesting something already
		// present, and needs to say which version this is instead.
		h.fail(w, r, http.StatusConflict, "duplicate_document", err.Error())
		return
	case errors.Is(err, legal.ErrDuplicateVersionLabel):
		// The label is taken by different bytes. Reported separately from the
		// checksum case because the fix differs: this one needs a human to say
		// what this version actually is.
		h.fail(w, r, http.StatusConflict, "duplicate_version_label", err.Error())
		return
	case errors.Is(err, ErrInvalidRequest):
		h.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	case strings.HasPrefix(err.Error(), "invalid extraction"):
		h.fail(w, r, http.StatusUnprocessableEntity, "invalid_extraction", err.Error())
		return
	default:
		h.log.ErrorContext(r.Context(), "ingestion failed",
			slog.String("title", ingestReq.Document.Title),
			slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "ingestion_failed", err.Error())
		return
	}

	h.respond(w, r, http.StatusCreated, result)
}

// toServiceRequest converts the wire shape into a service request.
//
// It validates only what is a wire concern — dates, which arrive as strings and
// have to be parsed before anything can look at them. Everything else (title,
// label, doc type, language) is checked by Request.Validate inside Ingest, so
// that callers which are not this handler get the same guarantees; it used to
// live here, and the ingest CLI would have silently inherited none of it.
func (r ingestRequest) toServiceRequest() (Request, error) {
	var problems []error

	publicationDate, err := parseDate("document.publication_date", r.Document.PublicationDate)
	if err != nil {
		problems = append(problems, err)
	}
	effectiveDate, err := parseDate("document.effective_date", r.Document.EffectiveDate)
	if err != nil {
		problems = append(problems, err)
	}
	effectiveFrom, err := parseDate("version.effective_from", r.Version.EffectiveFrom)
	if err != nil {
		problems = append(problems, err)
	}
	repealedAt, err := parseDate("version.repealed_at", r.Version.RepealedAt)
	if err != nil {
		problems = append(problems, err)
	}

	if len(problems) > 0 {
		return Request{}, errors.Join(problems...)
	}

	policy := PublishNever
	if r.Publish {
		policy = PublishAlways
	}

	return Request{
		Document: legal.Document{
			Title:            strings.TrimSpace(r.Document.Title),
			DocType:          legal.DocType(r.Document.DocType),
			LegalDomain:      r.Document.LegalDomain,
			IssuingAuthority: r.Document.IssuingAuthority,
			PublicationDate:  publicationDate,
			EffectiveDate:    effectiveDate,
			Language:         legal.Language(r.Document.Language),
			SourceURL:        r.Document.SourceURL,
			StorageKey:       r.Document.StorageKey,
		},
		Version: legal.Version{
			Label:         strings.TrimSpace(r.Version.Label),
			EffectiveFrom: effectiveFrom,
			RepealedAt:    repealedAt,
			SourceURL:     r.Document.SourceURL,
			StorageKey:    r.Document.StorageKey,
		},
		PublishPolicy:  policy,
		SourceChecksum: strings.TrimSpace(r.SourceChecksum),
		Replace:        r.Replace,
		Extraction:     r.Extraction,
	}, nil
}

// parseDate accepts an empty string as "unknown", which is a real and common
// state for Ethiopian legal metadata.
func parseDate(field, value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse(dateLayout, value)
	if err != nil {
		return nil, fmt.Errorf("%s must be YYYY-MM-DD, got %q", field, value)
	}
	return &t, nil
}

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		h.log.ErrorContext(r.Context(), "encode ingest response", slog.Any("error", err))
		http.Error(w, `{"error":{"code":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	h.respond(w, r, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": detail,
		},
	})
}
