package extractor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func makeXLSX(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	if err := f.SetCellValue("Sheet1", "A1", "Year"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "B1", "Loss"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "A2", 2024); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "B2", 12500); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "A3", 2023); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "B3", 8000); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestXLSX_ExtractsCellsAsTabSeparated(t *testing.T) {
	out, err := NewXLSX().Extract(makeXLSX(t))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(out, "Year\tLoss") {
		t.Errorf("header row: %q", out)
	}
	if !strings.Contains(out, "2024\t12500") {
		t.Errorf("data row: %q", out)
	}
	if !strings.Contains(out, "2023\t8000") {
		t.Errorf("data row: %q", out)
	}
}

func TestXLSX_EmptyBytes_NoError(t *testing.T) {
	out, err := NewXLSX().Extract(nil)
	if err != nil || out != "" {
		t.Errorf("got out=%q err=%v", out, err)
	}
}

func TestXLSX_NonXLSXBytes_Error(t *testing.T) {
	_, err := NewXLSX().Extract([]byte("not an xlsx"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "xlsx") {
		t.Errorf("error should mention xlsx: %v", err)
	}
}

func TestXLSX_RowCapEnforced(t *testing.T) {
	f := excelize.NewFile()
	// Write more rows than the cap so extraction must truncate.
	total := maxXLSXRows + 500
	for i := 1; i <= total; i++ {
		if err := f.SetCellValue("Sheet1", "A"+itoa(i), "row"); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}

	out, err := NewXLSX().Extract(buf.Bytes())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	lines := strings.Count(out, "\n")
	if lines > maxXLSXRows {
		t.Errorf("emitted %d rows, cap is %d", lines, maxXLSXRows)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
