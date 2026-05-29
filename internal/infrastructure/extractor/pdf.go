package extractor

import (
	"bytes"
	"fmt"
	"io"

	"github.com/ledongthuc/pdf"
)

const (
	maxPDFPages     = 2_000   // NumPage() is untrusted metadata
	maxPDFTextBytes = 8 << 20 // compressed streams expand; cap the output
)

type PDF struct{}

func NewPDF() *PDF { return &PDF{} }

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
		if _, err := io.WriteString(&buf, texts); err != nil {
			return "", fmt.Errorf("pdf: write: %w", err)
		}
		buf.WriteString("\n")
	}
	return buf.String(), nil
}
