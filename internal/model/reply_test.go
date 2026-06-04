package model

import (
	"strings"
	"testing"
)

func TestBuildMissingItemsReply_LossRunsShortfall(t *testing.T) {
	minVal := 5.0
	cl := Checklist{
		Required: []RequiredItem{{
			ID:          "loss_runs",
			Description: "Loss runs for the past 5 years",
			RequiresField: &RequiresField{
				Name:     "years_covered",
				Type:     FieldTypeNumber,
				MinValue: &minVal,
				Unit:     "years",
			},
		}},
	}
	doc := Document{ClassifiedAs: "loss_runs", ExtractedFields: map[string]any{"years_covered": 3.0}}
	s := Submission{ID: "sub1", Documents: []Document{doc}}
	missing := EvaluateChecklist(s, cl)

	inbound := Email{FromName: "Dana Smith", FromAddress: "dana@brokerage.example", MessageID: "<m1@x>"}
	reply := BuildMissingItemsReply(s, missing, inbound)

	const wantLine = "  - Loss runs for the past 5 years (covers only 3 years, need at least 5)\n"
	if !strings.Contains(reply.BodyText, wantLine) {
		t.Fatalf("reply body missing exact line.\nwant: %q\nbody:\n%s", wantLine, reply.BodyText)
	}
	if strings.Contains(reply.BodyText, "field") || strings.Contains(reply.BodyText, "years_covered") {
		t.Errorf("reply leaks internal jargon:\n%s", reply.BodyText)
	}
}
