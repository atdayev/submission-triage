package repository

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/database"
)

func TestLLMSpend_AccumulatesAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spend.db")
	log := logrus.NewEntry(logrus.New())
	ctx := context.Background()

	db, err := database.Open(ctx, path, log)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := database.Migrate(ctx, db, filepath.Join("..", "..", "migrations"), log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	r := NewLLMSpendRepository(db)
	if v, err := r.Today(ctx); err != nil || v != 0 {
		t.Fatalf("fresh day should be 0: got %v, err %v", v, err)
	}
	if err := r.Add(ctx, 0.50); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(ctx, 0.25); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.Today(ctx); v != 0.75 {
		t.Fatalf("today: got %v, want 0.75", v)
	}
	db.Close()

	// simulate a restart: reopen the same file, the running total must persist
	db2, err := database.Open(ctx, path, log)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db2.Close() })
	if v, _ := NewLLMSpendRepository(db2).Today(ctx); v != 0.75 {
		t.Fatalf("spend must survive restart: got %v, want 0.75", v)
	}
}
