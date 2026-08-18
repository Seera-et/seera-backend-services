package citations

import (
	"encoding/json"
	"testing"
	"time"
)

// The wire format is the contract with the frontend, and it must match
// rag.Citation's — both describe the same thing to the same client, one on the
// answer stream and one from GET /api/v1/citations/{id}. Without JSON tags Go
// emits PascalCase and the client needs two shapes for one concept.
func TestCitationSerialisesAsSnakeCase(t *testing.T) {
	effective := time.Date(1931, 7, 16, 0, 0, 0, 0, time.UTC)
	body, err := json.Marshal(Citation{
		ChunkID:       "chunk-1",
		ArticleID:     "article-1",
		VersionID:     "version-1",
		DocumentID:    "doc-1",
		DocumentTitle: "The 1931 Constitution of Ethiopia",
		ArticleNo:     "16",
		VersionLabel:  "1931",
		EffectiveFrom: &effective,
		Language:      "en",
		Text:          "The Emperor has the right to grant pardons.",
		PageNo:        4,
	})
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}

	for _, key := range []string{
		"chunk_id", "article_id", "version_id", "document_id",
		"document_title", "article_no", "version_label", "effective_from",
		"language", "text", "page_no", "char_start", "char_end",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing %q in %s", key, body)
		}
	}

	// PascalCase would mean the tags were dropped again.
	for _, key := range []string{"ChunkID", "DocumentTitle", "ArticleNo"} {
		if _, ok := got[key]; ok {
			t.Errorf("%q serialised as PascalCase — JSON tags are missing", key)
		}
	}

	if got["effective_from"] != "1931-07-16T00:00:00Z" {
		t.Errorf("effective_from = %v", got["effective_from"])
	}
}

// Absent metadata is common and legitimate in Ethiopian legal sources; it should
// disappear from the payload rather than arrive as null for the UI to special-case.
func TestCitationOmitsAbsentOptionalFields(t *testing.T) {
	body, err := json.Marshal(Citation{ChunkID: "chunk-1", DocumentTitle: "Doc", ArticleNo: "1"})
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}

	for _, key := range []string{"effective_from", "repealed_at", "source_url", "section_path"} {
		if _, ok := got[key]; ok {
			t.Errorf("%q should be omitted when empty", key)
		}
	}
}
