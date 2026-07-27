package model

import (
	"strings"
	"testing"
)

func TestRequiresReview(t *testing.T) {
	cases := []struct {
		name    string
		missing []MissingItem
		want    bool
	}{
		{"none", nil, false},
		{"only not-provided", []MissingItem{{Code: ReasonNotProvided}}, false},
		{"only field-shortfall", []MissingItem{{Code: ReasonFieldShortfall}}, false},
		{"has unreadable", []MissingItem{{Code: ReasonNotProvided}, {Code: ReasonUnreadable}}, true},
		{"has low-confidence", []MissingItem{{Code: ReasonLowConfidence}}, true},
	}
	for _, tc := range cases {
		if got := RequiresReview(tc.missing); got != tc.want {
			t.Errorf("%s: RequiresReview = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBrokerActionable(t *testing.T) {
	missing := []MissingItem{
		{ID: "np", Code: ReasonNotProvided},
		{ID: "un", Code: ReasonUnreadable},
		{ID: "lc", Code: ReasonLowConfidence},
		{ID: "fs", Code: ReasonFieldShortfall},
	}
	got := BrokerActionable(missing)
	if len(got) != 2 {
		t.Fatalf("BrokerActionable kept %d items, want 2 (not_provided + field_shortfall): %+v", len(got), got)
	}
	for _, m := range got {
		if m.Code == ReasonUnreadable || m.Code == ReasonLowConfidence {
			t.Errorf("BrokerActionable leaked an agency-side item: %+v", m)
		}
	}
}

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
			missing := EvaluateChecklist(s, cl, ChecklistOptions{})
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
	missing := EvaluateChecklist(Submission{}, cl, ChecklistOptions{})
	if len(missing) != 1 {
		t.Fatalf("want 1 missing, got %d", len(missing))
	}
	if missing[0].Reason != "document not provided" {
		t.Errorf("Reason: got %q", missing[0].Reason)
	}
	if missing[0].Code != ReasonNotProvided {
		t.Errorf("Code: got %q, want %q", missing[0].Code, ReasonNotProvided)
	}
}

func TestEvaluateChecklist_ConfidenceFloor(t *testing.T) {
	cl := Checklist{Required: []RequiredItem{{ID: "acord_125", Description: "ACORD 125"}}}
	const floor = 0.80

	// below the floor: unsatisfied, low_confidence, carrying the classifier's confidence
	below := []Document{{ClassifiedAs: "acord_125", Confidence: 0.60}}
	m := EvaluateChecklist(Submission{Documents: below}, cl, ChecklistOptions{ConfidenceFloor: floor})
	if len(m) != 1 || m[0].Code != ReasonLowConfidence {
		t.Fatalf("below-floor doc should be low_confidence missing, got %+v", m)
	}
	if m[0].Confidence != 0.60 {
		t.Errorf("missing item should carry the classifier confidence, got %v", m[0].Confidence)
	}

	// exactly at the floor satisfies (>=, matching the min_value convention)
	atFloor := []Document{{ClassifiedAs: "acord_125", Confidence: floor}}
	if m := EvaluateChecklist(Submission{Documents: atFloor}, cl, ChecklistOptions{ConfidenceFloor: floor}); len(m) != 0 {
		t.Fatalf("a doc exactly at the floor should satisfy, got %+v", m)
	}

	// above the floor satisfies
	above := []Document{{ClassifiedAs: "acord_125", Confidence: 0.95}}
	if m := EvaluateChecklist(Submission{Documents: above}, cl, ChecklistOptions{ConfidenceFloor: floor}); len(m) != 0 {
		t.Fatalf("a doc above the floor should satisfy, got %+v", m)
	}

	// a qualifying doc satisfies even when a below-floor duplicate is also present
	mixed := []Document{
		{ClassifiedAs: "acord_125", Confidence: 0.60},
		{ClassifiedAs: "acord_125", Confidence: 0.95},
	}
	if m := EvaluateChecklist(Submission{Documents: mixed}, cl, ChecklistOptions{ConfidenceFloor: floor}); len(m) != 0 {
		t.Fatalf("a qualifying duplicate should clear the item, got %+v", m)
	}
}

func TestEvaluateChecklist_UnreadableCode(t *testing.T) {
	cl := Checklist{Required: []RequiredItem{{ID: "loss_runs", Description: "Loss runs"}}}
	docs := []Document{{ClassifiedAs: "loss_runs", Confidence: 0.95, Unreadable: true}}
	m := EvaluateChecklist(Submission{Documents: docs}, cl, ChecklistOptions{ConfidenceFloor: 0.80})
	if len(m) != 1 || m[0].Code != ReasonUnreadable {
		t.Fatalf("an unreadable doc should be unreadable missing regardless of confidence, got %+v", m)
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
			missing := EvaluateChecklist(s, cl, ChecklistOptions{})
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
	missing := EvaluateChecklist(Submission{Documents: docs}, cl, ChecklistOptions{})
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
	if missing := EvaluateChecklist(Submission{Documents: docs}, cl, ChecklistOptions{}); len(missing) != 0 {
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
	missing := EvaluateChecklist(Submission{Documents: below}, cl, ChecklistOptions{})
	if len(missing) != 1 || !strings.Contains(missing[0].Reason, "covers only 3 years, need at least 5") {
		t.Fatalf("string '3' should coerce and fail: %v", missing)
	}

	ok := []Document{{ClassifiedAs: "loss_runs", ExtractedFields: map[string]any{"years_covered": "7"}}}
	if m := EvaluateChecklist(Submission{Documents: ok}, cl, ChecklistOptions{}); len(m) != 0 {
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
	if m := EvaluateChecklist(Submission{Documents: bad}, cl, ChecklistOptions{}); len(m) != 1 {
		t.Fatalf("non-numeric value for a number field should fail, got %v", m)
	}
	good := []Document{{ClassifiedAs: "doc", ExtractedFields: map[string]any{"count": 2.0}}}
	if m := EvaluateChecklist(Submission{Documents: good}, cl, ChecklistOptions{}); len(m) != 0 {
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
	missing := EvaluateChecklist(Submission{Documents: docs}, cl, ChecklistOptions{})
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
	if m := EvaluateChecklist(Submission{Documents: docs}, cl, ChecklistOptions{}); len(m) != 1 {
		t.Fatalf("NaN must not satisfy the minimum, got %v", m)
	}
}
