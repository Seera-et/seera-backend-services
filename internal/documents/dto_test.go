package documents

import (
	"net/url"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

func TestParseListFilter(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		want    ListFilter
		wantErr bool
	}{
		{
			name:  "empty query takes the defaults",
			query: "",
			want:  ListFilter{Limit: defaultPageSize},
		},
		{
			name:  "all filters",
			query: "q=commercial&language=am&type=code&limit=5&offset=10",
			want: ListFilter{
				Term:     "commercial",
				Language: legal.LanguageAmharic,
				DocType:  legal.DocTypeCode,
				Limit:    5,
				Offset:   10,
			},
		},
		{
			name:  "term is trimmed",
			query: "q=%20%20labour%20%20",
			want:  ListFilter{Term: "labour", Limit: defaultPageSize},
		},
		{
			// Silently ignoring it would look like a corpus missing documents.
			name:    "unknown language is rejected",
			query:   "language=fr",
			wantErr: true,
		},
		{
			name:    "unknown doc type is rejected",
			query:   "type=memo",
			wantErr: true,
		},
		{
			name:    "limit above the cap is rejected",
			query:   "limit=1000",
			wantErr: true,
		},
		{
			name:    "limit of zero is rejected",
			query:   "limit=0",
			wantErr: true,
		},
		{
			name:    "non-numeric limit is rejected",
			query:   "limit=all",
			wantErr: true,
		},
		{
			name:    "negative offset is rejected",
			query:   "offset=-1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("bad test query: %v", err)
			}

			got, err := ParseListFilter(values)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseListFilter(%q) = %+v, want error", tt.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseListFilter(%q): %v", tt.query, err)
			}
			if got != tt.want {
				t.Errorf("ParseListFilter(%q) = %+v, want %+v", tt.query, got, tt.want)
			}
		})
	}
}

func TestParseArticleFilter(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		want    ArticleFilter
		wantErr bool
	}{
		{
			name:  "defaults to the published version from the start",
			query: "",
			want:  ArticleFilter{Limit: defaultArticlePageSize},
		},
		{
			name:  "explicit version and cursor",
			query: "version=" + sampleUUID + "&after=50&limit=25",
			want: ArticleFilter{
				VersionID: legal.VersionID(sampleUUID),
				After:     50,
				Limit:     25,
			},
		},
		{
			name:    "limit above the cap is rejected",
			query:   "limit=5000",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("bad test query: %v", err)
			}

			got, err := ParseArticleFilter(values)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseArticleFilter(%q) = %+v, want error", tt.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArticleFilter(%q): %v", tt.query, err)
			}
			if got != tt.want {
				t.Errorf("ParseArticleFilter(%q) = %+v, want %+v", tt.query, got, tt.want)
			}
		})
	}
}

const sampleUUID = "3f1c4d2e-0a5b-4c7d-9e1f-2b3a4c5d6e7f"
