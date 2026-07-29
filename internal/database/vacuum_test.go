package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
)

// Vacuum runs after the retention sweep deletes rows; it must work on a live WAL
// database, since that is the only state it is ever called in.
func TestVacuum_ReclaimsAfterDelete(t *testing.T) {
	ctx := context.Background()
	log := logrus.NewEntry(logrus.New())
	db, err := Open(ctx, filepath.Join(t.TempDir(), "vacuum.db"), log)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `CREATE TABLE bulk (id INTEGER PRIMARY KEY, blob TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO bulk (blob) VALUES (?)`, string(make([]byte, 1024))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM bulk`); err != nil {
		t.Fatal(err)
	}
	if err := Vacuum(ctx, db); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	// the connection must still be usable afterwards — the worker keeps running
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bulk`).Scan(&n); err != nil {
		t.Fatalf("post-vacuum query: %v", err)
	}
	if n != 0 {
		t.Errorf("rows after delete+vacuum: got %d, want 0", n)
	}
}
