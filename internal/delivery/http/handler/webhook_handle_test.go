package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
)

func newHandlerWithSecret(secret string) *WebhookHandler {
	return &WebhookHandler{
		svc: nil,
		cfg: config.PostmarkConfig{WebhookSecret: secret},
		log: logrus.NewEntry(logrus.New()),
	}
}

func TestHandle_Unauthorized_NoSecretProvided(t *testing.T) {
	h := newHandlerWithSecret("topsecret")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid webhook credentials") {
		t.Errorf("body: %q", rec.Body.String())
	}
}

func TestHandle_Unauthorized_WrongSecret(t *testing.T) {
	h := newHandlerWithSecret("topsecret")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark", bytes.NewBufferString(`{}`))
	req.Header.Set(webhookSecretHeader, "wrong")
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

func TestHandle_AcceptsSecretViaHeader_ThenFailsOnBodyParse(t *testing.T) {
	h := newHandlerWithSecret("topsecret")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark", bytes.NewBufferString(`not json`))
	req.Header.Set(webhookSecretHeader, "topsecret")
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (auth passed, body bad)", rec.Code)
	}
}

func TestHandle_RejectsSecretViaQueryString(t *testing.T) {
	// query-string secrets leak into proxy/CDN logs; header is the only accepted path
	h := newHandlerWithSecret("topsecret")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark?secret=topsecret", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401 (query-string secret should not be accepted)", rec.Code)
	}
}

func TestHandle_NoSecretConfigured_SkipsAuthCheck(t *testing.T) {
	h := newHandlerWithSecret("")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark", bytes.NewBufferString(`not json`))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	// Auth is bypassed; body still rejected as malformed JSON.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestHandle_EmptyBody_Rejected(t *testing.T) {
	h := newHandlerWithSecret("")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark", bytes.NewBufferString(""))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}
