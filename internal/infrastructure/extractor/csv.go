package extractor

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

const (
	maxCSVRows = 100_000 // emitted (non-blank) rows kept
	maxCSVScan = 500_000 // total rows read, so a blank/empty-row flood can't loop unbounded
)

// CSV flattens a sheet to tab-joined rows.
type CSV struct{}

// NewCSV returns a CSV text extractor.
func NewCSV() *CSV { return &CSV{} }

// Extract returns the CSV rows as tab-joined lines, capped by row and scan limits.
func (e *CSV) Extract(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	var out strings.Builder
	emitted, scanned := 0, 0
	for emitted < maxCSVRows && scanned < maxCSVScan {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("csv: read: %w", err)
		}
		scanned++
		line := strings.TrimSpace(strings.Join(row, "\t"))
		if line == "" {
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
		emitted++
	}
	return out.String(), nil
}
