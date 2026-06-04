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

func TestGreetingName(t *testing.T) {
	cases := []struct {
		name string
		in   Email
		want string
	}{
		{"first token of display name", Email{FromName: "Dana Smith", FromAddress: "dana@agency.example"}, "Dana"},
		{"single-token display name", Email{FromName: "Dana"}, "Dana"},
		{"clean local-part when no display name", Email{FromAddress: "dana@agency.example"}, "dana"},
		{"display name beats messy local-part", Email{FromName: "Dana Smith", FromAddress: "noreply@agency.example"}, "Dana"},
		{"local-part with digits falls back", Email{FromAddress: "atdayevdemo2@gmail.com"}, "there"},
		{"short local-part falls back", Email{FromAddress: "ab@x.example"}, "there"},
		{"role submissions falls back", Email{FromAddress: "submissions@agency.example"}, "there"},
		{"role info falls back", Email{FromAddress: "info@agency.example"}, "there"},
		{"role noreply falls back", Email{FromAddress: "noreply@agency.example"}, "there"},
		{"role no-reply falls back", Email{FromAddress: "no-reply@agency.example"}, "there"},
		{"role support falls back", Email{FromAddress: "support@agency.example"}, "there"},
		{"empty email falls back", Email{}, "there"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := greetingName(tc.in); got != tc.want {
				t.Errorf("greetingName(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
