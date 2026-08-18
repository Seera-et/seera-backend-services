package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is an arbitrary but fixed key for the Postgres advisory lock
// that serialises migration runs across concurrently starting instances.
const migrationLockID int64 = 8_675_309

// Migration is one forward schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrate applies every migration that has not yet run, in version order.
//
// Migrations are forward-only. There are no down migrations: rolling a schema
// backwards in a system whose whole premise is that legal history is never
// destroyed is a footgun, and a bad migration is fixed by writing the next one.
// The .up.sql suffix is kept purely so the directory stays compatible with
// golang-migrate if this runner is ever outgrown.
//
// Each migration runs in its own transaction alongside its bookkeeping row, so
// a failure leaves the schema exactly as it was before that migration started.
func Migrate(ctx context.Context, pool *Pool, log *slog.Logger) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	// Serialise across instances. Released automatically when the session ends,
	// so a crashed migrator does not wedge every future boot.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
			log.Error("release migration lock", slog.Any("error", err))
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer     PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn.Conn())
	if err != nil {
		return err
	}

	var ran int
	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		if err := applyMigration(ctx, conn.Conn(), m); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
		}
		log.Info("migration applied",
			slog.Int("version", m.Version),
			slog.String("name", m.Name))
		ran++
	}

	if ran == 0 {
		log.Info("schema up to date", slog.Int("migrations", len(migrations)))
	}
	return nil
}

func applyMigration(ctx context.Context, conn *pgx.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO schema_migrations (version, name) VALUES ($1, $2)",
		m.Version, m.Name); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit(ctx)
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// loadMigrations reads the embedded migrations and returns them in version
// order. Filenames are NNNN_name.up.sql; anything else is an error rather than
// a silent skip, because a migration that does not run is worse than one that
// fails loudly.
func loadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var migrations []Migration
	seen := make(map[int]string)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %s and %s", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		migrations = append(migrations, Migration{Version: version, Name: name, SQL: string(body)})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	return migrations, nil
}

func parseMigrationName(filename string) (version int, name string, err error) {
	base := strings.TrimSuffix(filename, ".sql")
	base = strings.TrimSuffix(base, ".up")

	num, rest, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("migration %q: expected NNNN_name.up.sql", filename)
	}
	version, err = strconv.Atoi(num)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q: %q is not a version number", filename, num)
	}
	if version <= 0 {
		return 0, "", fmt.Errorf("migration %q: version must be positive", filename)
	}
	return version, rest, nil
}
