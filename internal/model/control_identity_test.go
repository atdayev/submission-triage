package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/atdayev/submission-triage/internal/model"
)

// TestReasonCodeTable pins the Fix Spec 4 reason-code table: which codes are
// broker-actionable and which raise the human-review flag.
func TestReasonCodeTable(t *testing.T) {
	cases := []struct {
		code             model.ReasonCode
		brokerActionable bool
		raisesReview     bool
	}{
		{model.ReasonNotProvided, true, false},
		{model.ReasonFieldShortfall, true, false},
		{model.ReasonEncrypted, true, false},
		{model.ReasonMissingAttachment, true, false},
		{model.ReasonUnreadable, false, true},
		{model.ReasonLowConfidence, false, true},
	}
	for _, c := range cases {
		items := []model.MissingItem{{ID: "x", Code: c.code}}
		if ba := len(model.BrokerActionable(items)) == 1; ba != c.brokerActionable {
			t.Errorf("%s: BrokerActionable=%v, want %v", c.code, ba, c.brokerActionable)
		}
		if rr := model.RequiresReview(items); rr != c.raisesReview {
			t.Errorf("%s: RequiresReview=%v, want %v", c.code, rr, c.raisesReview)
		}
	}
}

func TestEvaluateChecklist_EncryptedDocIsBrokerActionable(t *testing.T) {
	cl := model.Checklist{PolicyType: "cgl", Required: []model.RequiredItem{{ID: "acord_125", Description: "ACORD 125"}}}
	sub := model.Submission{Documents: []model.Document{{ClassifiedAs: "acord_125", Confidence: 0.95, Encrypted: true}}}
	missing := model.EvaluateChecklist(sub, cl, model.ChecklistOptions{ConfidenceFloor: 0.80})
	if len(missing) != 1 || missing[0].Code != model.ReasonEncrypted {
		t.Fatalf("an encrypted classified doc should be missing as encrypted, got %+v", missing)
	}
	if model.RequiresReview(missing) {
		t.Error("an encrypted document must not raise the review flag")
	}
	if len(model.BrokerActionable(missing)) != 1 {
		t.Error("an encrypted document is broker-actionable")
	}
}

func TestBuildDigest_LeadsWithNamedInsuredAndFlagSections(t *testing.T) {
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	eff := now.Add(3 * 24 * time.Hour)
	subs := []model.Submission{
		{ID: "1", State: model.StateAwaiting, PolicyType: "cgl", NamedInsured: "Acme Manufacturing", SubjectLine: "New Business Submission", CreatedAt: now, LastActionAt: now, EffectiveDate: &eff},
		{ID: "2", State: model.StateAwaiting, PolicyType: "cgl", SubjectLine: "unreachable", DeliveryFailed: true, CreatedAt: now, LastActionAt: now},
		{ID: "3", State: model.StateAwaiting, PolicyType: "cgl", SubjectLine: "worked by a person", OnHold: true, CreatedAt: now, LastActionAt: now},
	}
	out := model.BuildDigest(subs, nil, nil, 0, now)
	if !strings.Contains(out, "Acme Manufacturing") {
		t.Error("digest should lead with the named insured, not the generic subject")
	}
	if strings.Contains(out, "New Business Submission") {
		t.Error("digest should not show the generic subject when a named insured is known")
	}
	if !strings.Contains(out, "Delivery failed — address unreachable") {
		t.Error("delivery-failed heading missing")
	}
	if !strings.Contains(out, "paused by a human") {
		t.Error("held heading missing")
	}
	if !strings.Contains(out, "binds in 3d") {
		t.Error("bind-window day count missing from the row")
	}
}
