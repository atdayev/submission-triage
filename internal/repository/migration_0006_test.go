package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/database"
)

// Migration 0006 must collapse any pre-existing multiple pending rows per
// submission (keeping the newest) before it can create the one-pending unique
// index — otherwise a live upgrade would abort or drop an owed reply.
func TestMigration0006_CollapsesDuplicatePendingRows(t *testing.T) {
	ctx := context.Background()
	log := logrus.NewEntry(logrus.New())
	realMig := filepath.Join("..", "..", "migrations")

	// stage 1: apply everything before coalescing (0001-0005), i.e. before the
	// one-pending unique index exists, so duplicate pending rows can be seeded
	stage1 := t.TempDir()
	for _, name := range []string{
		"0001_init.sql", "0002_extracted_fields.sql", "0003_outbox.sql",
		"0004_document_unreadable.sql", "0005_llm_spend.sql",
	} {
		body, err := os.ReadFile(filepath.Join(realMig, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(stage1, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "mig.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(ctx, db, stage1, log); err != nil {
		t.Fatalf("stage1 migrate: %v", err)
	}

	// seed a submission + two pending outbox rows for it (allowed pre-0006)
	now := time.Now().UTC().UnixNano()
	if _, err := db.ExecContext(ctx, `INSERT INTO submissions (id, policy_type, state, created_at, updated_at, last_action_at) VALUES ('s1','cgl','awaiting',?,?,?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{`{"BodyText":"old"}`, `{"BodyText":"new"}`} {
		if _, err := db.ExecContext(ctx, `INSERT INTO outbox (id, submission_id, reply_json, status, attempts, last_error, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("ob-%d", i), "s1", body, "pending", 0, "", now+int64(i), now+int64(i)); err != nil {
			t.Fatal(err)
		}
	}

	// stage 2: apply 0006 (dedup DELETE + partial unique index) from the real dir
	if err := database.Migrate(ctx, db, realMig, log); err != nil {
		t.Fatalf("stage2 migrate (0006): %v", err)
	}

	// exactly one pending row survives — the newest (highest rowid)
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE submission_id='s1' AND status='pending'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("0006 should collapse to one pending row, got %d", count)
	}
	var survivor string
	if err := db.QueryRowContext(ctx, `SELECT reply_json FROM outbox WHERE submission_id='s1' AND status='pending'`).Scan(&survivor); err != nil {
		t.Fatal(err)
	}
	if survivor != `{"BodyText":"new"}` {
		t.Errorf("the newest reply should survive the dedup, got %s", survivor)
	}

	// the partial unique index exists
	var idx string
	if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND name='idx_outbox_one_pending'`).Scan(&idx); err != nil {
		t.Fatalf("idx_outbox_one_pending not created: %v", err)
	}
}
