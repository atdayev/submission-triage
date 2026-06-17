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
			Unit:     "years",
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
			reasonHint:  "covers only 3 years, need at least 5",
		},
		{
			name:        "field value explicitly nil fails",
			fields:      map[string]any{"years_covered": nil},
			wantMissing: true,
			reasonHint:  "could not confirm the number of years covered",
		},
		{
			name:        "field value non-numeric fails",
			fields:      map[string]any{"years_covered": "five"},
			wantMissing: true,
			reasonHint:  "could not confirm the number of years covered",
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
			if tc.wantMissing {
				if r := missing[0].Reason; strings.Contains(r, "field") || strings.Contains(r, "years_covered") {
					t.Errorf("Reason leaks internal jargon: %q", r)
				}
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

func TestEvaluateChecklist_AnyDocSatisfiesField(t *testing.T) {
	minVal := 5.0
	cl := Checklist{Required: []RequiredItem{{
		ID:            "loss_runs",
		Description:   "Loss runs",
		RequiresField: &RequiresField{Name: "years_covered", Type: FieldTypeNumber, MinValue: &minVal, Unit: "years"},
	}}}
	// the failing doc is listed first; a later satisfying doc must clear the item
	docs := []Document{
		{ClassifiedAs: "loss_runs", ExtractedFields: map[string]any{"years_covered": 3.0}},
		{ClassifiedAs: "loss_runs", ExtractedFields: map[string]any{"years_covered": 7.0}},
	}
	if missing := EvaluateChecklist(Submission{Documents: docs}, cl); len(missing) != 0 {
		t.Fatalf("a satisfying duplicate should clear the item, got %v", missing)
	}
}

func TestEvaluateChecklist_NumericStringCoercion(t *testing.T) {
	minVal := 5.0
	cl := Checklist{Required: []RequiredItem{{
		ID:            "loss_runs",
		Description:   "Loss runs",
		RequiresField: &RequiresField{Name: "years_covered", Type: FieldTypeNumber, MinValue: &minVal, Unit: "years"},
	}}}

	below := []Document{{ClassifiedAs: "loss_runs", ExtractedFields: map[string]any{"years_covered": "3"}}}
	missing := EvaluateChecklist(Submission{Documents: below}, cl)
	if len(missing) != 1 || !strings.Contains(missing[0].Reason, "covers only 3 years, need at least 5") {
		t.Fatalf("string '3' should coerce and fail: %v", missing)
	}

	ok := []Document{{ClassifiedAs: "loss_runs", ExtractedFields: map[string]any{"years_covered": "7"}}}
	if m := EvaluateChecklist(Submission{Documents: ok}, cl); len(m) != 0 {
		t.Fatalf("string '7' should coerce and pass: %v", m)
	}
}

func TestEvaluateChecklist_NumberTypeEnforcedWithoutMin(t *testing.T) {
	cl := Checklist{Required: []RequiredItem{{
		ID:            "doc",
		Description:   "A doc",
		RequiresField: &RequiresField{Name: "count", Type: FieldTypeNumber},
	}}}
	bad := []Document{{ClassifiedAs: "doc", ExtractedFields: map[string]any{"count": "abc"}}}
	if m := EvaluateChecklist(Submission{Documents: bad}, cl); len(m) != 1 {
		t.Fatalf("non-numeric value for a number field should fail, got %v", m)
	}
	good := []Document{{ClassifiedAs: "doc", ExtractedFields: map[string]any{"count": 2.0}}}
	if m := EvaluateChecklist(Submission{Documents: good}, cl); len(m) != 0 {
		t.Fatalf("numeric value should pass, got %v", m)
	}
}

func TestFormatNum_NoScientificNotation(t *testing.T) {
	cases := map[float64]string{
		3:    "3",
		5:    "5",
		3.5:  "3.5",
		1e21: "1000000000000000000000",
		1e-7: "0.0000001",
	}
	for in, want := range cases {
		if got := formatNum(in); got != want {
			t.Errorf("formatNum(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestEvaluateChecklist_SoftPassDoesNotMaskFailure(t *testing.T) {
	minVal := 5.0
	cl := Checklist{Required: []RequiredItem{{
		ID:            "loss_runs",
		Description:   "Loss runs",
		RequiresField: &RequiresField{Name: "years_covered", Type: FieldTypeNumber, MinValue: &minVal, Unit: "years"},
	}}}
	// one matching doc concretely falls short (3<5); another never had extraction
	// run (nil map = soft-pass). The soft-pass must NOT satisfy the item.
	docs := []Document{
		{ClassifiedAs: "loss_runs", ExtractedFields: map[string]any{"years_covered": 3.0}},
		{ClassifiedAs: "loss_runs"},
	}
	missing := EvaluateChecklist(Submission{Documents: docs}, cl)
	if len(missing) != 1 || !strings.Contains(missing[0].Reason, "covers only 3 years, need at least 5") {
		t.Fatalf("soft-pass masked the shortfall: %v", missing)
	}
}

func TestNumericValue_RejectsNonFinite(t *testing.T) {
	for _, s := range []string{"NaN", "Inf", "+Inf", "-Inf"} {
		if _, ok := numericValue(s); ok {
			t.Errorf("numericValue(%q) should be rejected", s)
		}
	}
	if f, ok := numericValue("3"); !ok || f != 3 {
		t.Errorf("numericValue(\"3\") = %v,%v; want 3,true", f, ok)
	}
}

func TestEvaluateChecklist_NaNValueDoesNotSatisfyMinimum(t *testing.T) {
	minVal := 5.0
	cl := Checklist{Required: []RequiredItem{{
		ID:            "loss_runs",
		Description:   "Loss runs",
		RequiresField: &RequiresField{Name: "years_covered", Type: FieldTypeNumber, MinValue: &minVal, Unit: "years"},
	}}}
	docs := []Document{{ClassifiedAs: "loss_runs", ExtractedFields: map[string]any{"years_covered": "NaN"}}}
	if m := EvaluateChecklist(Submission{Documents: docs}, cl); len(m) != 1 {
		t.Fatalf("NaN must not satisfy the minimum, got %v", m)
	}
}
