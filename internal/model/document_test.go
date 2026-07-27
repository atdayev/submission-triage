package model

import (
	"reflect"
	"testing"
)

func TestFilenameOnlyMatches(t *testing.T) {
	tests := []struct {
		name string
		docs []Document
		want []string
	}{
		{
			name: "filename alone is flagged",
			docs: []Document{{ClassifiedAs: "acord_125", ClassifiedBy: ClassifiedByFilename}},
			want: []string{"acord_125"},
		},
		{
			name: "keyword corroboration clears the flag",
			docs: []Document{{ClassifiedAs: "acord_125", ClassifiedBy: ClassifiedByKeyword}},
			want: nil,
		},
		{
			name: "filename plus keyword is not filename-only",
			docs: []Document{{ClassifiedAs: "acord_125", ClassifiedBy: ClassifiedByFilenameKeyword}},
			want: nil,
		},
		{
			name: "llm match is not filename-only",
			docs: []Document{{ClassifiedAs: "loss_runs", ClassifiedBy: ClassifiedByLLM}},
			want: nil,
		},
		{
			name: "same item matched by filename and separately by keyword is corroborated",
			docs: []Document{
				{ClassifiedAs: "loss_runs", ClassifiedBy: ClassifiedByFilename},
				{ClassifiedAs: "loss_runs", ClassifiedBy: ClassifiedByKeyword},
			},
			want: nil,
		},
		{
			name: "unclassified docs are ignored",
			docs: []Document{
				{ClassifiedAs: "", ClassifiedBy: ClassifiedByHeuristic},
				{ClassifiedAs: "acord_125", ClassifiedBy: ClassifiedByFilename},
			},
			want: []string{"acord_125"},
		},
		{
			name: "results are sorted",
			docs: []Document{
				{ClassifiedAs: "loss_runs", ClassifiedBy: ClassifiedByFilename},
				{ClassifiedAs: "acord_125", ClassifiedBy: ClassifiedByFilename},
			},
			want: []string{"acord_125", "loss_runs"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilenameOnlyMatches(tt.docs)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FilenameOnlyMatches = %v, want %v", got, tt.want)
			}
		})
	}
}
