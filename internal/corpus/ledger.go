package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Status is what became of one document in an ingest run.
type Status string

const (
	StatusPublished  Status = "published"   // ingested and live
	StatusDraft      Status = "draft"       // ingested, held back by a gate
	StatusSkipped    Status = "skipped"     // nothing to do
	StatusQuarantine Status = "quarantined" // refused before ingestion
	StatusFailed     Status = "failed"      // something broke
)

// Terminal reports whether this document needs no further work, so a second run
// can skip it.
func (s Status) Terminal() bool {
	return s == StatusPublished || s == StatusDraft || s == StatusSkipped
}

// Outcome is the ledger entry for one document.
//
// It lives beside the crawler's own records rather than inside them, and that
// separation is deliberate. crawler.Record.State drives the crawler's -resume,
// and State.Terminal() covers READY_FOR_INGESTION and FAILED — so writing an
// ingestion failure into it would make the next crawl skip the document
// permanently, removing it from acquisition because its text extracted badly.
// Record is also rewritten whole on every save, so a ten-minute ingest holding
// a loaded copy would clobber anything the crawler wrote meanwhile.
//
// Keeping a separate ledger also keeps the stages independent: ingestion can be
// re-run without re-crawling, and the ledger records *why* a document was held
// back, which the database cannot say.
type Outcome struct {
	DocID    string `json:"doc_id"`
	Checksum string `json:"checksum,omitempty"`
	Status   Status `json:"status"`

	// Reason is why a document was skipped, quarantined or left as a draft.
	Reason string `json:"reason,omitempty"`

	DocumentID string `json:"document_id,omitempty"`
	VersionID  string `json:"version_id,omitempty"`
	Articles   int    `json:"articles,omitempty"`
	Chunks     int    `json:"chunks,omitempty"`

	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

// Ledger is the ingest run's record, one file per document under
// <data>/ingest/.
type Ledger struct{ dir string }

// OpenLedger creates or opens the ledger under a store root.
func OpenLedger(root string) (*Ledger, error) {
	dir := filepath.Join(root, "ingest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create ingest ledger: %w", err)
	}
	return &Ledger{dir: dir}, nil
}

func (l *Ledger) path(docID string) string { return filepath.Join(l.dir, docID+".json") }

// Load reads one document's outcome. Reports false when there is none.
func (l *Ledger) Load(docID string) (Outcome, bool, error) {
	body, err := os.ReadFile(l.path(docID))
	if os.IsNotExist(err) {
		return Outcome{}, false, nil
	}
	if err != nil {
		return Outcome{}, false, fmt.Errorf("read ledger entry %s: %w", docID, err)
	}

	var o Outcome
	if err := json.Unmarshal(body, &o); err != nil {
		return Outcome{}, false, fmt.Errorf("decode ledger entry %s: %w", docID, err)
	}
	return o, true, nil
}

// Save writes one document's outcome, replacing any previous one.
//
// Written after every attempt, including failures: a run interrupted after
// forty documents did forty documents of work, and the next run needs to know
// that without asking the database forty times.
func (l *Ledger) Save(o Outcome) error {
	o.At = time.Now().UTC()

	body, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ledger entry %s: %w", o.DocID, err)
	}

	// Via a temporary file and a rename, so an interrupted write cannot leave a
	// half-written entry that the next run fails to parse. Same reasoning as
	// crawler.Store.
	tmp, err := os.CreateTemp(l.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp ledger entry: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("write ledger entry %s: %w", o.DocID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close ledger entry %s: %w", o.DocID, err)
	}
	return os.Rename(tmpName, l.path(o.DocID))
}
