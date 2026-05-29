package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func TestRecovery_PanicReturns500AndLogs(t *testing.T) {
	lg, hook := test.NewNullLogger()
	log := logrus.NewEntry(lg)

	panicker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	chain := withRequestID(log)(withRecovery()(panicker))

	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}

	foundPanic := false
	for _, e := range hook.AllEntries() {
		if e.Message == "recovered panic" {
			foundPanic = true
		}
	}
	if !foundPanic {
		t.Error("expected 'recovered panic' log entry")
	}
}

func TestRequestID_ValidUUIDHonored(t *testing.T) {
	lg := logrus.New()
	lg.SetOutput(discardWriter{})
	log := logrus.NewEntry(lg)
	const rid = "12345678-1234-1234-1234-123456789012"

	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	chain := withRequestID(log)(noop)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", rid)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-ID"); got != rid {
		t.Errorf("X-Request-ID: got %q, want %q (valid UUID should pass through)", got, rid)
	}
}

func TestRequestID_InvalidHeaderReplaced(t *testing.T) {
	lg := logrus.New()
	lg.SetOutput(discardWriter{})
	log := logrus.NewEntry(lg)

	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	chain := withRequestID(log)(noop)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "not-a-uuid; drop table users")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	got := rec.Header().Get("X-Request-ID")
	if got == "" {
		t.Fatal("X-Request-ID should be set in response")
	}
	if strings.Contains(got, "drop table") {
		t.Errorf("malicious header propagated: %q", got)
	}
	// Should be a regenerated UUID — 36 chars with hyphens at the right spots.
	if len(got) != 36 || got[8] != '-' || got[13] != '-' || got[18] != '-' || got[23] != '-' {
		t.Errorf("expected UUID-shape replacement, got %q", got)
	}
}

func TestRequestID_NoHeaderGeneratesOne(t *testing.T) {
	lg := logrus.New()
	lg.SetOutput(discardWriter{})
	log := logrus.NewEntry(lg)

	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	chain := withRequestID(log)(noop)

	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("expected generated X-Request-ID in response")
	}
}

type discardWriter struct{}

func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }
