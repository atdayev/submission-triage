package extractor

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

const (
	maxXLSXRows      = 100_000   // zip-compressed; small file, huge expansion
	maxUnzipBytes    = 256 << 20 // total decompressed size cap (zip-bomb guard)
	maxUnzipXMLBytes = 64 << 20  // per-XML-part decompressed cap
)

// XLSX extracts plain text from .xlsx spreadsheets.
type XLSX struct{}

// NewXLSX returns an XLSX text extractor.
func NewXLSX() *XLSX { return &XLSX{} }

// Extract returns the sheet rows as tab-joined lines, capped by row and unzip limits.
func (e *XLSX) Extract(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	f, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{
		UnzipSizeLimit:    maxUnzipBytes,
		UnzipXMLSizeLimit: maxUnzipXMLBytes,
	})
	if err != nil {
		if f != nil {
			f.Close()
		}
		return "", fmt.Errorf("xlsx: open: %w", err)
	}
	defer f.Close()

	var out strings.Builder
	emitted := 0
	for _, sheet := range f.GetSheetList() {
		if emitted >= maxXLSXRows {
			break
		}
		// stream rows; GetRows would materialize the whole sheet
		rows, err := f.Rows(sheet)
		if err != nil {
			continue
		}
		for rows.Next() {
			if emitted >= maxXLSXRows {
				break
			}
			cols, err := rows.Columns()
			if err != nil {
				continue
			}
			line := strings.TrimSpace(strings.Join(cols, "\t"))
			if line == "" {
				continue
			}
			out.WriteString(line)
			out.WriteString("\n")
			emitted++
		}
		_ = rows.Close()
	}
	return out.String(), nil
}
