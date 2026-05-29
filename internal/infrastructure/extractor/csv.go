package extractor

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

const maxCSVRows = 100_000

// CSV flattens a sheet to tab-joined rows.
type CSV struct{}

func NewCSV() *CSV { return &CSV{} }

func (e *CSV) Extract(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	var out strings.Builder
	for i := 0; i < maxCSVRows; i++ {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("csv: read: %w", err)
		}
		line := strings.TrimSpace(strings.Join(row, "\t"))
		if line == "" {
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String(), nil
}
