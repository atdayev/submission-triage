package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/database"
)

func TestHealth_DBUp_NoPostmark_ReturnsOK(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewHealthHandler(db, config.PostmarkConfig{}, log)
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"db":"ok"`) {
		t.Errorf(`expected "db":"ok", got %s`, body)
	}
	if !strings.Contains(body, `"postmark":"not_configured"`) {
		t.Errorf(`expected postmark not_configured, got %s`, body)
	}
}

func TestHealth_DBDown_Returns503(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "h2.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // intentionally close before serving

	h := NewHealthHandler(db, config.PostmarkConfig{}, log)
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}
