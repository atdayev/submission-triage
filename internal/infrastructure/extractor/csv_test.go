package extractor

import (
	"strings"
	"testing"
)

func TestCSV_EmptyRowsDoNotExhaustEmitBudget(t *testing.T) {
	// encoding/csv returns delimiter-bearing empty rows (unlike bare blank lines,
	// which it skips); they must not count against the emit cap, so a later data
	// row still survives
	var b strings.Builder
	for i := 0; i < maxCSVRows+100; i++ {
		b.WriteString(",\n")
	}
	b.WriteString("x,y\n")

	out, err := NewCSV().Extract([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "x\ty") {
		t.Errorf("data row dropped after empty rows: %q", out)
	}
}

func TestCSV_Extract(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
		wantErr bool
	}{
		{
			name:    "simple csv",
			input:   "policy,year,limit\nGL,2023,1000000\nAuto,2024,500000\n",
			wantSub: "GL\t2023\t1000000",
		},
		{
			name:    "blank rows skipped",
			input:   "a,b\n\n\n,\nx,y\n",
			wantSub: "x\ty",
		},
		{
			name:    "empty input returns empty",
			input:   "",
			wantSub: "",
		},
		{
			name:    "ragged rows accepted (fields per record disabled)",
			input:   "a,b,c\nx,y\nl,m,n,o\n",
			wantSub: "l\tm\tn\to",
		},
		{
			name: "quoted fields with commas",
			input: `name,note
"Acme, Inc.","since 2010"
`,
			wantSub: "Acme, Inc.\tsince 2010",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewCSV().Extract([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
				t.Errorf("output missing %q: got %q", tc.wantSub, got)
			}
			if tc.wantSub == "" && got != "" {
				t.Errorf("expected empty output, got %q", got)
			}
		})
	}
}
