package store

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/arkfile/SubscriptionBridge/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplyMigrations applies pending SQL migrations in one transaction.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := listMigrations()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS sb_schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return err
	}
	for _, f := range files {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM sb_schema_migrations WHERE version=$1`, f.version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := tx.Exec(ctx, f.sql); err != nil {
			return fmt.Errorf("migration %d: %w", f.version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sb_schema_migrations(version) VALUES ($1)`, f.version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type migFile struct {
	version int
	sql     string
}

// listMigrations loads numbered migration files from the embedded FS.
func listMigrations() ([]migFile, error) {
	var files []migFile
	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".sql") {
			return err
		}
		raw, err := fs.ReadFile(migrations.FS, path)
		if err != nil {
			return err
		}
		base := d.Name()
		num := strings.SplitN(base, "_", 2)[0]
		ver, err := strconv.Atoi(num)
		if err != nil {
			ver = 1
		}
		files = append(files, migFile{version: ver, sql: string(raw)})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, err
}

// CheckSchema requires the applied schema version to match CurrentSchemaVersion.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var v int
	err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version),0) FROM sb_schema_migrations`).Scan(&v)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSchemaMismatch, err)
	}
	if v != CurrentSchemaVersion {
		return fmt.Errorf("%w: have %d want %d", ErrSchemaMismatch, v, CurrentSchemaVersion)
	}
	return nil
}
