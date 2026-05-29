package utils

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atdayev/submission-triage/pkg/apperror"
	"github.com/atdayev/submission-triage/pkg/logger"
)

func TestWriteJSON_SetsContentTypeAndBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	WriteJSON(rec, req, http.StatusOK, map[string]string{"k": "v"})

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

func TestWriteJSONError_AttachesRequestIDFromContext(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(logger.ContextWithRequestID(req.Context(), "rid-42"))

	WriteJSONError(rec, req, http.StatusBadRequest,
		apperror.NewErrorResponse(apperror.CodeInvalidPayload, "oops"))

	var resp apperror.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RequestID != "rid-42" {
		t.Errorf("request_id: got %q, want rid-42", resp.RequestID)
	}
	if resp.Code != apperror.CodeInvalidPayload {
		t.Errorf("code: got %s", resp.Code)
	}
}

func TestWriteJSONError_NilResponseFallsBackToInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	WriteJSONError(rec, req, http.StatusInternalServerError, nil)

	var resp apperror.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != apperror.CodeInternal {
		t.Errorf("code: got %s", resp.Code)
	}
}

func TestDecodeJSON_HappyPath(t *testing.T) {
	body := bytes.NewBufferString(`{"x": 1}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()

	var dst struct {
		X int `json:"x"`
	}
	if err := DecodeJSON(rec, req, &dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dst.X != 1 {
		t.Fatalf("got %+v", dst)
	}
}

func TestDecodeJSON_EmptyBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer(nil))
	rec := httptest.NewRecorder()
	var dst map[string]any
	err := DecodeJSON(rec, req, &dst)
	if err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("expected empty-body error, got %v", err)
	}
}

func TestDecodeJSON_InvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString("not json"))
	rec := httptest.NewRecorder()
	var dst map[string]any
	if err := DecodeJSON(rec, req, &dst); err == nil {
		t.Fatal("expected error for invalid json")
	}
}
