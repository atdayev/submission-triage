package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

const goodMigration = `CREATE TABLE good (id INTEGER PRIMARY KEY);`

// 0002_bad starts with a valid CREATE TABLE then runs a syntax error.
// In a non-transactional implementation, the first statement would commit
// before the second fails, leaving the bad_first table behind permanently.
// Our applyMigration wraps the whole body in a tx, so the rollback should
// leave the database with only the good table.
const badMigration = `CREATE TABLE bad_first (id INTEGER PRIMARY KEY);
THIS IS DELIBERATELY NOT VALID SQL;`

func setupMigDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0001_good.sql"), []byte(goodMigration), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0002_bad.sql"), []byte(badMigration), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(t.TempDir(), "test.db")
}

func TestMigrate_FailedMigrationRollsBackEntireFile(t *testing.T) {
	migDir, dbPath := setupMigDir(t)
	log := logrus.NewEntry(logrus.New())
	ctx := context.Background()

	db, err := Open(ctx, dbPath, log)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = Migrate(ctx, db, migDir, log)
	if err == nil {
		t.Fatal("expected migration error from 0002_bad")
	}
	if !strings.Contains(err.Error(), "0002") {
		t.Errorf("error should reference 0002: %v", err)
	}

	// 0001 must have applied.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM good").Scan(&n); err != nil {
		t.Errorf("good table should exist after 0001: %v", err)
	}

	// 0002 must have fully rolled back: bad_first should not exist.
	row := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='bad_first'")
	var name string
	if err := row.Scan(&name); err == nil {
		t.Errorf("bad_first table leaked despite rollback: %q", name)
	}

	// schema_migrations should record 0001_good but NOT 0002_bad.
	rows, err := db.Query("SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var applied []string
	for rows.Next() {
		var v string
		rows.Scan(&v)
		applied = append(applied, v)
	}
	if len(applied) != 1 || applied[0] != "0001_good" {
		t.Errorf("schema_migrations: got %v, want [0001_good]", applied)
	}
}

func TestMigrate_ReRunRetriesFailedMigration(t *testing.T) {
	migDir, dbPath := setupMigDir(t)
	log := logrus.NewEntry(logrus.New())
	ctx := context.Background()

	db, _ := Open(ctx, dbPath, log)
	defer db.Close()

	// First run: 0002 fails.
	if err := Migrate(ctx, db, migDir, log); err == nil {
		t.Fatal("expected first run to fail")
	}

	// Replace 0002 with a fixed version.
	fixed := `CREATE TABLE bad_first (id INTEGER PRIMARY KEY);`
	if err := os.WriteFile(filepath.Join(migDir, "0002_bad.sql"), []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-run: must retry 0002 (not skipped because no row was written for it).
	if err := Migrate(ctx, db, migDir, log); err != nil {
		t.Fatalf("re-run after fix: %v", err)
	}
	row := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='bad_first'")
	var name string
	if err := row.Scan(&name); err != nil {
		t.Errorf("bad_first should exist after re-run: %v", err)
	}
}
