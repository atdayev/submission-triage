package extractor

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

const maxXLSXRows = 100_000 // zip-compressed; small file, huge expansion

type XLSX struct{}

func NewXLSX() *XLSX { return &XLSX{} }

func (e *XLSX) Extract(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("xlsx: open: %w", err)
	}
	defer f.Close()

	var out strings.Builder
	emitted := 0
	for _, sheet := range f.GetSheetList() {
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if emitted >= maxXLSXRows {
				return out.String(), nil
			}
			line := strings.TrimSpace(strings.Join(row, "\t"))
			if line == "" {
				continue
			}
			out.WriteString(line)
			out.WriteString("\n")
			emitted++
		}
	}
	return out.String(), nil
}
