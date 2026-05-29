package extractor

import (
	"strings"
	"testing"
)

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
