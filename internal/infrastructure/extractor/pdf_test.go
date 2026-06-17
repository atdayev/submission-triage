package extractor

import (
	"strings"
	"testing"
)

func TestPDF_NonPDFBytes_Error(t *testing.T) {
	_, err := NewPDF().Extract([]byte("this is not a pdf, just text"))
	if err == nil {
		t.Fatal("expected error for non-PDF bytes")
	}
	if !strings.Contains(err.Error(), "not a pdf") {
		t.Errorf("error: %v", err)
	}
}

func TestPDF_EmptyBytes_NoError(t *testing.T) {
	out, err := NewPDF().Extract(nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty, got %q", out)
	}
}

func TestPDF_MalformedAfterHeader_Error(t *testing.T) {
	// starts with %PDF but immediately invalid
	_, err := NewPDF().Extract([]byte("%PDF-1.4\nthis is garbage not a real pdf"))
	if err == nil {
		t.Fatal("expected error for malformed pdf")
	}
}
