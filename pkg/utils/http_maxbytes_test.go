package utils

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bigStringReader emits an unterminated, huge JSON string to trip MaxBytesReader.
type bigStringReader struct {
	wrote  int64
	target int64
	prefix string
}

func (b *bigStringReader) Read(p []byte) (int, error) {
	if b.wrote == 0 && b.prefix != "" {
		n := copy(p, b.prefix)
		b.wrote += int64(n)
		return n, nil
	}
	if b.wrote >= b.target {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > b.target-b.wrote {
		n = int(b.target - b.wrote)
	}
	for i := 0; i < n; i++ {
		p[i] = 'a'
	}
	b.wrote += int64(n)
	return n, nil
}

func TestDecodeJSON_BodyOverCap_Rejected(t *testing.T) {
	body := &bigStringReader{
		prefix: `{"x":"`,
		target: int64(MaxInboundBodyBytes) + 1024,
	}
	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()

	var dst map[string]any
	err := DecodeJSON(rec, req, &dst)
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention size: %v", err)
	}
}
