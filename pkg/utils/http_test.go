package utils

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
