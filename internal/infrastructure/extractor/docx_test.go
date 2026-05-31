package extractor

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// buildDOCX returns a minimal .docx-shaped zip whose word/document.xml
// holds the given paragraph strings as <w:p><w:r><w:t> nodes.
func buildDOCX(t *testing.T, paragraphs ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	doc := `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`
	for _, p := range paragraphs {
		doc += `<w:p><w:r><w:t>` + p + `</w:t></w:r></w:p>`
	}
	doc += `</w:body></w:document>`
	if _, err := w.Write([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDOCX_ExtractsText(t *testing.T) {
	data := buildDOCX(t, "Loss Run 2024", "Acme Insurance Co.")
	out, err := NewDOCX().Extract(data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(out, "Loss Run 2024") {
		t.Errorf("missing first paragraph: %q", out)
	}
	if !strings.Contains(out, "Acme Insurance Co.") {
		t.Errorf("missing second paragraph: %q", out)
	}
}

func TestDOCX_ParagraphSeparation(t *testing.T) {
	data := buildDOCX(t, "alpha", "beta")
	out, err := NewDOCX().Extract(data)
	if err != nil {
		t.Fatal(err)
	}
	// Each <w:p> emits a newline before its content.
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("missing content: %q", out)
	}
	if strings.Index(out, "alpha") > strings.Index(out, "beta") {
		t.Errorf("order wrong: %q", out)
	}
}

func TestDOCX_EmptyBytes_NoError(t *testing.T) {
	out, err := NewDOCX().Extract(nil)
	if err != nil || out != "" {
		t.Errorf("got out=%q err=%v", out, err)
	}
}

func TestDOCX_NonZipBytes_Error(t *testing.T) {
	_, err := NewDOCX().Extract([]byte("not a zip"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "docx") {
		t.Errorf("error should mention docx: %v", err)
	}
}

func TestDOCX_NoDocumentXML_ReturnsEmpty(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("some/other/file.xml")
	w.Write([]byte("<w:t>not the right path</w:t>"))
	zw.Close()

	out, err := NewDOCX().Extract(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("expected empty (no word/document.xml), got %q", out)
	}
}

func TestDOCX_PartTooLarge_RejectedBySizeGuard(t *testing.T) {
	// document.xml whose uncompressed size exceeds the cap (small compressed, large inflated)
	huge := strings.Repeat("X", maxDocxPartBytes+1024)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	w.Write([]byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + huge + `</w:t></w:r></w:p></w:body></w:document>`))
	zw.Close()

	_, err := NewDOCX().Extract(buf.Bytes())
	if err == nil {
		t.Fatal("expected zip-bomb guard to reject oversized part")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention size: %v", err)
	}
}
