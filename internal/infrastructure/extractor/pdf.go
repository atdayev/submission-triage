package extractor

import (
	"bytes"
	"fmt"
	"io"

	"github.com/ledongthuc/pdf"

	"github.com/atdayev/submission-triage/pkg/textutil"
)

const (
	maxPDFPages     = 2_000   // NumPage() is untrusted metadata
	maxPDFTextBytes = 8 << 20 // compressed streams expand; cap the output
)

// PDF extracts plain text from PDF documents.
type PDF struct{}

// NewPDF returns a PDF text extractor.
func NewPDF() *PDF { return &PDF{} }

// Extract returns the plain text of a PDF, capped by page and byte limits.
func (e *PDF) Extract(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		return "", fmt.Errorf("pdf: not a pdf file")
	}
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("pdf: open: %w", err)
	}
	var buf bytes.Buffer
	pageCount := r.NumPage()
	if pageCount > maxPDFPages {
		pageCount = maxPDFPages
	}
	for i := 1; i <= pageCount; i++ {
		if buf.Len() >= maxPDFTextBytes {
			break
		}
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		texts, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}
		if remaining := maxPDFTextBytes - buf.Len(); len(texts) > remaining {
			texts = textutil.TruncateBytes(texts, remaining)
		}
		if _, err := io.WriteString(&buf, texts); err != nil {
			return "", fmt.Errorf("pdf: write: %w", err)
		}
		if buf.Len() >= maxPDFTextBytes {
			break
		}
		buf.WriteString("\n")
	}
	return buf.String(), nil
}
