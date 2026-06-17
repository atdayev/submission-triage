package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/database"
)

func TestHealth_DBUp_ReturnsOK(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewHealthHandler(db, log)
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"db":"ok"`) {
		t.Errorf(`expected "db":"ok", got %s`, body)
	}
}

func TestHealth_DBDown_Returns503(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "h2.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // intentionally close before serving

	h := NewHealthHandler(db, log)
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"db":"down"`) {
		t.Errorf(`expected generic "db":"down", got %s`, body)
	}
	if strings.Contains(body, "down:") || strings.Contains(body, "sql:") {
		t.Errorf("raw db error leaked to client: %s", body)
	}
}

func TestWriteJSON_SetsContentTypeAndBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	writeJSON(rec, req, http.StatusOK, map[string]string{"k": "v"})

	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type: got %q", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["k"] != "v" {
		t.Errorf("body: got %+v", body)
	}
}
