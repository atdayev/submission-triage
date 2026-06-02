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
