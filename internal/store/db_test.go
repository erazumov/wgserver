package store

import (
	"path/filepath"
	"testing"
)

func TestOpen_RunsMigrationsOnFreshDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wgserver.sqlite")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version < 1 {
		t.Fatalf("schema_version = %d, want >= 1", version)
	}

	for _, table := range []string{
		"admins", "peers", "telegram_users", "telegram_claims", "schema_version",
	} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&n); err != nil {
			t.Fatalf("query sqlite_master for %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %q present = %d, want 1", table, n)
		}
	}
}

func TestOpen_IdempotentMigrations(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wgserver.sqlite")

	db1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	_ = db1.Close()

	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	var version int
	if err := db2.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}

	var count int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&count); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if count != version {
		t.Errorf("schema_version rows = %d, want equal to MAX(version)=%d (no duplicates)", count, version)
	}
}

func TestOpen_AppliesMigrationsInOrder(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wgserver.sqlite")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.Query(`SELECT version FROM schema_version ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	defer rows.Close()

	prev := 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v <= prev {
			t.Errorf("schema versions not strictly increasing: prev=%d got=%d", prev, v)
		}
		prev = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if prev == 0 {
		t.Errorf("no schema versions recorded")
	}
}
