package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/config"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func newHMACHandler(sigSecret, sharedSecret string) *WebhookHandler {
	return &WebhookHandler{
		svc: nil,
		cfg: config.PostmarkConfig{
			WebhookSecret:          sharedSecret,
			WebhookSignatureSecret: sigSecret,
		},
		log: logrus.NewEntry(logrus.New()),
	}
}

func TestHMAC_ValidSignature_Accepted(t *testing.T) {
	const secret = "topsecret"
	body := `not-valid-json` // valid JSON shape, will fail downstream but auth must pass
	h := newHMACHandler(secret, "")

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set(webhookSignatureHeader, sign(secret, body))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	// Auth passes; we'd panic on h.svc.IngestEmail (svc is nil) so we
	// reach the JSON decode path successfully and panic when ingesting.
	// What we care about: NOT 401.
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("expected auth to pass, got 401: %s", rec.Body.String())
	}
}

func TestHMAC_PrefixedSignature_Accepted(t *testing.T) {
	const secret = "topsecret"
	body := `not-valid-json`
	h := newHMACHandler(secret, "")

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set(webhookSignatureHeader, "sha256="+sign(secret, body))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("expected sha256= prefix to be accepted, got 401: %s", rec.Body.String())
	}
}

func TestHMAC_WrongSignature_Rejected(t *testing.T) {
	body := `not-valid-json`
	h := newHMACHandler("topsecret", "")

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set(webhookSignatureHeader, sign("wrongsecret", body))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHMAC_MissingSignature_Rejected(t *testing.T) {
	body := `not-valid-json`
	h := newHMACHandler("topsecret", "")

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	// no header
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHMAC_TamperedBody_Rejected(t *testing.T) {
	const secret = "topsecret"
	originalBody := `not-valid-json-original`
	tamperedBody := `not-valid-json-tampered`
	h := newHMACHandler(secret, "")

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(tamperedBody))
	req.Header.Set(webhookSignatureHeader, sign(secret, originalBody))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered body, got %d", rec.Code)
	}
}

func TestAuth_EitherMechanismGrantsAccess(t *testing.T) {
	body := `not-valid-json`
	h := newHMACHandler("sigsecret", "sharedsecret")

	// Pass via shared header, no HMAC.
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set(webhookSecretHeader, "sharedsecret")
	rec := httptest.NewRecorder()
	h.Handle(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("shared secret alone should grant access: %d", rec.Code)
	}

	// Pass via HMAC alone, no shared header.
	req2 := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req2.Header.Set(webhookSignatureHeader, sign("sigsecret", body))
	rec2 := httptest.NewRecorder()
	h.Handle(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Errorf("HMAC alone should grant access: %d", rec2.Code)
	}

	// Neither: rejected.
	req3 := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	rec3 := httptest.NewRecorder()
	h.Handle(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Errorf("neither credential should be 401, got %d", rec3.Code)
	}
}
