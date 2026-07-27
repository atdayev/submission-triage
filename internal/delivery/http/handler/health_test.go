package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/database"
)

// fakePoll is a canned PollStatus for health tests.
type fakePoll struct {
	last       time.Time
	configured bool
}

func (f fakePoll) LastSuccessfulPoll() time.Time { return f.last }
func (f fakePoll) Configured() bool              { return f.configured }

const testStaleAfter = 90 * time.Second // 3 x a 30s interval

func TestHealth_DBUp_ReturnsOK(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewHealthHandler(db, nil, testStaleAfter, log)
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

	h := NewHealthHandler(db, nil, testStaleAfter, log)
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

func TestHealth_FreshPoll_ReturnsOK(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "h3.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewHealthHandler(db, fakePoll{last: time.Now(), configured: true}, testStaleAfter, log)
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"imap":"ok"`) {
		t.Errorf(`expected "imap":"ok", got %s`, body)
	}
}

func TestHealth_StalePoll_Returns503(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "h4.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	stale := fakePoll{last: time.Now().Add(-4 * testStaleAfter), configured: true}
	h := NewHealthHandler(db, stale, testStaleAfter, log)
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"imap":"stale"`) {
		t.Errorf(`expected "imap":"stale", got %s`, body)
	}
	if !strings.Contains(body, `"db":"ok"`) {
		t.Errorf("db should still read ok on a stale-only failure: %s", body)
	}
}

func TestHealth_NilPoll_ReportsNotConfigured(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "h5.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewHealthHandler(db, nil, testStaleAfter, log)
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"imap":"not_configured"`) {
		t.Errorf(`expected "imap":"not_configured", got %s`, body)
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
