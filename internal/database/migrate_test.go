package database

import "testing"

func TestParseMigrationName(t *testing.T) {
	for _, tc := range []struct {
		filename    string
		wantVersion int
		wantName    string
		wantErr     bool
	}{
		{filename: "0001_extensions.up.sql", wantVersion: 1, wantName: "extensions"},
		{filename: "0012_legal_documents.up.sql", wantVersion: 12, wantName: "legal_documents"},
		{filename: "0002_chunks.sql", wantVersion: 2, wantName: "chunks"},
		{filename: "extensions.up.sql", wantErr: true},
		{filename: "abcd_extensions.up.sql", wantErr: true},
		{filename: "0000_zero.up.sql", wantErr: true},
	} {
		t.Run(tc.filename, func(t *testing.T) {
			version, name, err := parseMigrationName(tc.filename)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMigrationName(%q) succeeded, want an error", tc.filename)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMigrationName(%q) = %v", tc.filename, err)
			}
			if version != tc.wantVersion || name != tc.wantName {
				t.Errorf("got (%d, %q), want (%d, %q)", version, name, tc.wantVersion, tc.wantName)
			}
		})
	}
}

// A migration that fails to load is a schema that silently never arrives, so
// the embedded set is checked at test time rather than at 3am on a deploy.
func TestLoadMigrationsReadsEmbeddedSetInOrder(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() = %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations were embedded")
	}

	for i, m := range migrations {
		if i > 0 && m.Version <= migrations[i-1].Version {
			t.Errorf("migrations out of order at %d: %d after %d", i, m.Version, migrations[i-1].Version)
		}
		if m.SQL == "" {
			t.Errorf("migration %04d_%s is empty", m.Version, m.Name)
		}
	}

	if migrations[0].Version != 1 || migrations[0].Name != "extensions" {
		t.Errorf("first migration = %04d_%s, want 0001_extensions",
			migrations[0].Version, migrations[0].Name)
	}
}
