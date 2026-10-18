package crawler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record is everything the crawler knows about one document.
//
// Written to metadata/<doc-id>.json after every state change, so an interrupted
// crawl resumes from what is on disk rather than from the beginning. It is also
// the input the ingestion step reads for provenance: source URL, original file
// URL and checksum are what let a stored article be traced back to the file it
// came from.
type Record struct {
	// DocID is the law page slug — stable, unique per law, and readable in a
	// directory listing, which a hash would not be.
	DocID string `json:"doc_id"`

	State  State         `json:"state"`
	Reason FailureReason `json:"failure_reason,omitempty"`
	Error  string        `json:"error,omitempty"`

	Page LawPage `json:"page"`

	// OriginalFileURL is the PDF that was actually downloaded — the resolved
	// one, never a jet_download endpoint and never a chrome-extension wrapper.
	OriginalFileURL string `json:"original_file_url,omitempty"`
	PDFSource       string `json:"pdf_source,omitempty"`
	Checksum        string `json:"checksum,omitempty"`
	Bytes           int64  `json:"bytes,omitempty"`

	// DuplicateOf names the document that already held this checksum, when the
	// same PDF is reachable from more than one law page.
	DuplicateOf string `json:"duplicate_of,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

// Fail marks a record failed, with a category and the underlying error.
func (r *Record) Fail(reason FailureReason, err error) {
	r.State = StateFailed
	r.Reason = reason
	if err != nil {
		r.Error = err.Error()
	}
}

// Store is the crawler's on-disk layout.
//
// A directory tree rather than a database table, deliberately: acquisition runs
// standalone (see this package's doc comment), the artefacts are files, and
// nothing here needs a query beyond "have I seen this checksum". Ingestion is
// where the database starts.
type Store struct {
	root string

	// mu guards the checksum index, which the concurrent fetchers share.
	mu        sync.Mutex
	checksums map[string]string // sha256 → doc id
}

// Directory names under the store root.
const (
	dirRaw        = "raw"
	dirExtracted  = "extracted"
	dirStructured = "structured"
	dirMetadata   = "metadata"
	dirIndex      = "index"
)

const checksumIndexFile = "checksums.json"

// NewStore opens (or creates) a store rooted at a directory, loading any
// checksum index left by an earlier run.
func NewStore(root string) (*Store, error) {
	s := &Store{root: root, checksums: map[string]string{}}

	for _, dir := range []string{dirRaw, dirExtracted, dirStructured, dirMetadata, dirIndex} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := s.loadChecksums(); err != nil {
		return nil, err
	}
	return s, nil
}

// maxDocIDRunes caps the readable half of a document id.
//
// The Ministry's slugs are the law's Amharic title, and they run long:
// "የኢትዮጵያ-ስታርታፕ-አዋጅ-ቁጥር-1396-2017-ethiopian-startup-procla". Percent-encoded
// on disk they are longer still, and Windows' default MAX_PATH of 260 is
// reachable once the store root and subdirectories are added.
const maxDocIDRunes = 60

// docIDHashLen is how much of the slug's digest is appended. Eight hex
// characters distinguish every document in a corpus of this size, and keep the
// id short enough to read.
const docIDHashLen = 8

// unsafeInFilename matches characters that cannot appear in a path segment on
// the platforms this runs on: path separators, Windows' reserved set, and
// control characters.
var unsafeInFilename = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]`)

// SafeDocID turns a law page slug into a directory name.
//
// Two problems make this more than cosmetic. The slug is a URL path segment
// that has already been percent-decoded, so it can contain characters — a
// separator, "..", a Windows-reserved character — that would write outside the
// store or fail to create at all. And Ministry slugs are long Amharic titles
// that can push a path past Windows' MAX_PATH once the store root is prepended.
//
// The result keeps a readable prefix so a directory listing is still navigable,
// and appends a digest of the *full* slug so two laws whose titles share their
// first sixty characters — which Ethiopian proclamation titles routinely do —
// never collide.
func SafeDocID(slug string) string {
	cleaned := unsafeInFilename.ReplaceAllString(slug, "-")
	cleaned = strings.Trim(strings.TrimSpace(cleaned), ".-")

	runes := []rune(cleaned)
	if len(runes) > maxDocIDRunes {
		runes = runes[:maxDocIDRunes]
	}
	cleaned = strings.Trim(string(runes), ".-")

	sum := sha256.Sum256([]byte(slug))
	digest := hex.EncodeToString(sum[:])[:docIDHashLen]

	if cleaned == "" {
		// A slug that sanitised away to nothing still needs a stable home.
		return digest
	}
	return cleaned + "-" + digest
}

// PDFPath is where a document's original PDF belongs.
func (s *Store) PDFPath(docID string) string {
	return filepath.Join(s.root, dirRaw, docID, "original.pdf")
}

// TextPath is where workers/extraction writes the extracted plain text.
func (s *Store) TextPath(docID string) string {
	return filepath.Join(s.root, dirExtracted, docID, "text.txt")
}

// StructuredPath is where the ingestion.Extraction JSON belongs — the contract
// internal/ingestion already reads.
func (s *Store) StructuredPath(docID string) string {
	return filepath.Join(s.root, dirStructured, docID, "document.json")
}

func (s *Store) recordPath(docID string) string {
	return filepath.Join(s.root, dirMetadata, docID+".json")
}

// ListRecords reads every record in the store, ordered by document id.
//
// The crawler itself never needs this — it works from a discovery list and
// looks records up by id. Ingestion does: its unit of work is "every document
// this store holds", and the store is the only place that knows what was
// acquired.
//
// Ordered so a run's report, and which of two documents is processed first, is
// the same on every machine.
func (s *Store) ListRecords() ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, dirMetadata))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read metadata directory: %w", err)
	}

	records := make([]Record, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		docID := strings.TrimSuffix(e.Name(), ".json")

		// A record that will not parse is reported, not skipped: silently
		// ignoring it would drop a document from the corpus with nothing said.
		r, found, err := s.LoadRecord(docID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		records = append(records, r)
	}

	sort.Slice(records, func(i, j int) bool { return records[i].DocID < records[j].DocID })
	return records, nil
}

// SaveRecord writes a document's record, replacing any previous one.
func (s *Store) SaveRecord(r Record) error {
	r.UpdatedAt = time.Now().UTC()

	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode record %s: %w", r.DocID, err)
	}
	return writeFileAtomic(s.recordPath(r.DocID), body)
}

// LoadRecord reads a document's record. Reports false when there is none.
func (s *Store) LoadRecord(docID string) (Record, bool, error) {
	body, err := os.ReadFile(s.recordPath(docID))
	if os.IsNotExist(err) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read record %s: %w", docID, err)
	}

	var r Record
	if err := json.Unmarshal(body, &r); err != nil {
		return Record{}, false, fmt.Errorf("decode record %s: %w", docID, err)
	}
	return r, true, nil
}

// ClaimChecksum registers a checksum against a document.
//
// Returns the document that already held it, if any — the caller then knows
// this PDF is a duplicate reachable from a second law page and should not be
// ingested twice. The Ministry does this routinely: an amending proclamation
// and the law it amends can link the same consolidated file.
func (s *Store) ClaimChecksum(sum, docID string) (owner string, duplicate bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.checksums[sum]; ok && existing != docID {
		return existing, true
	}
	s.checksums[sum] = docID
	return docID, false
}

// SaveChecksums persists the index. Called once at the end of a run rather than
// on every claim: it is a rebuildable cache, and rewriting it per document
// would be the crawl's busiest write for no benefit.
func (s *Store) SaveChecksums() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Sorted so the file diffs cleanly between runs.
	keys := make([]string, 0, len(s.checksums))
	for k := range s.checksums {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ordered := make(map[string]string, len(keys))
	for _, k := range keys {
		ordered[k] = s.checksums[k]
	}

	body, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checksum index: %w", err)
	}
	return writeFileAtomic(filepath.Join(s.root, dirIndex, checksumIndexFile), body)
}

func (s *Store) loadChecksums() error {
	body, err := os.ReadFile(filepath.Join(s.root, dirIndex, checksumIndexFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read checksum index: %w", err)
	}
	if err := json.Unmarshal(body, &s.checksums); err != nil {
		return fmt.Errorf("decode checksum index: %w", err)
	}
	return nil
}

// writeFileAtomic writes via a temporary file and a rename, so a crash cannot
// leave a half-written record that the next run would fail to parse.
func writeFileAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return os.Rename(tmpName, path)
}
