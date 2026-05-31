package model

import (
	"strings"
	"testing"
)

func TestEvaluateChecklist(t *testing.T) {
	cl := Checklist{
		Name:       "Test",
		PolicyType: "cgl",
		Required: []RequiredItem{
			{ID: "a", Description: "A doc"},
			{ID: "b", Description: "B doc"},
			{ID: "c", Description: "C doc"},
		},
	}

	cases := []struct {
		name        string
		docs        []Document
		wantMissing []string
	}{
		{
			name:        "all missing",
			docs:        nil,
			wantMissing: []string{"a", "b", "c"},
		},
		{
			name: "two satisfied, one missing",
			docs: []Document{
				{ClassifiedAs: "a"},
				{ClassifiedAs: "b"},
			},
			wantMissing: []string{"c"},
		},
		{
			name: "all satisfied",
			docs: []Document{
				{ClassifiedAs: "a"},
				{ClassifiedAs: "b"},
				{ClassifiedAs: "c"},
			},
			wantMissing: []string{},
		},
		{
			name: "unclassified docs ignored",
			docs: []Document{
				{ClassifiedAs: ""},
				{ClassifiedAs: "a"},
				{ClassifiedAs: "b"},
				{ClassifiedAs: "c"},
			},
			wantMissing: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Submission{Documents: tc.docs}
			missing := EvaluateChecklist(s, cl)
			got := make([]string, 0, len(missing))
			for _, m := range missing {
				got = append(got, m.ID)
			}
			if len(got) != len(tc.wantMissing) {
				t.Fatalf("got %v, want %v", got, tc.wantMissing)
			}
			for i := range got {
				if got[i] != tc.wantMissing[i] {
					t.Fatalf("got %v, want %v", got, tc.wantMissing)
				}
			}
		})
	}
}

func TestEvaluateChecklist_MissingItem_HasReason(t *testing.T) {
	cl := Checklist{Required: []RequiredItem{{ID: "a", Description: "A doc"}}}
	missing := EvaluateChecklist(Submission{}, cl)
	if len(missing) != 1 {
		t.Fatalf("want 1 missing, got %d", len(missing))
	}
	if missing[0].Reason != "document not provided" {
		t.Errorf("Reason: got %q", missing[0].Reason)
	}
}

func TestEvaluateChecklist_RequiresField(t *testing.T) {
	minVal := 5.0
	item := RequiredItem{
		ID:          "loss_runs",
		Description: "Loss runs",
		RequiresField: &RequiresField{
			Name:     "years_covered",
			Type:     FieldTypeNumber,
			MinValue: &minVal,
		},
	}
	cl := Checklist{Required: []RequiredItem{item}}

	cases := []struct {
		name        string
		fields      map[string]any
		wantMissing bool
		reasonHint  string
	}{
		{
			name:        "no extraction attempted (nil map) soft-passes",
			fields:      nil,
			wantMissing: false,
		},
		{
			name:        "key absent from map soft-passes",
			fields:      map[string]any{"other_field": 1.0},
			wantMissing: false,
		},
		{
			name:        "field value above minimum passes",
			fields:      map[string]any{"years_covered": 7.0},
			wantMissing: false,
		},
		{
			name:        "field value equal to minimum passes",
			fields:      map[string]any{"years_covered": 5.0},
			wantMissing: false,
		},
		{
			name:        "field value below minimum fails",
			fields:      map[string]any{"years_covered": 3.0},
			wantMissing: true,
			reasonHint:  "needs at least",
		},
		{
			name:        "field value explicitly nil fails",
			fields:      map[string]any{"years_covered": nil},
			wantMissing: true,
			reasonHint:  "not found in document",
		},
		{
			name:        "field value non-numeric fails",
			fields:      map[string]any{"years_covered": "five"},
			wantMissing: true,
			reasonHint:  "not numeric",
		},
		{
			name:        "integer value accepted",
			fields:      map[string]any{"years_covered": 6},
			wantMissing: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := Document{ClassifiedAs: "loss_runs", ExtractedFields: tc.fields}
			s := Submission{Documents: []Document{doc}}
			missing := EvaluateChecklist(s, cl)
			if tc.wantMissing && len(missing) != 1 {
				t.Fatalf("want 1 missing, got %d (%v)", len(missing), missing)
			}
			if !tc.wantMissing && len(missing) != 0 {
				t.Fatalf("want 0 missing, got %d (%v)", len(missing), missing)
			}
			if tc.reasonHint != "" && !strings.Contains(missing[0].Reason, tc.reasonHint) {
				t.Errorf("Reason missing hint %q: got %q", tc.reasonHint, missing[0].Reason)
			}
		})
	}
}

func TestEvaluateChecklist_RequiresField_DocNotClassifiedIsStillMissing(t *testing.T) {
	minVal := 5.0
	cl := Checklist{Required: []RequiredItem{{
		ID:            "loss_runs",
		Description:   "Loss runs",
		RequiresField: &RequiresField{Name: "years_covered", Type: FieldTypeNumber, MinValue: &minVal},
	}}}
	// document classified as something else: still missing as not-provided, not a field reason
	docs := []Document{{ClassifiedAs: "other", ExtractedFields: map[string]any{"years_covered": 10.0}}}
	missing := EvaluateChecklist(Submission{Documents: docs}, cl)
	if len(missing) != 1 {
		t.Fatalf("want 1 missing, got %d", len(missing))
	}
	if missing[0].Reason != "document not provided" {
		t.Errorf("Reason: got %q, want %q", missing[0].Reason, "document not provided")
	}
}
