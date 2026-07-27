package model

import (
	"strings"
	"testing"
	"time"
)

func TestBuildDigest_GroupsByStateInPriorityOrder(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	subs := []Submission{
		{ID: "aw", State: StateAwaiting, PolicyType: "cgl", SubjectLine: "Awaiting one", FromAddress: "a@x", CreatedAt: old, LastActionAt: old},
		{ID: "co", State: StateComplete, PolicyType: "cgl", SubjectLine: "Complete one", FromAddress: "c@x", CreatedAt: old, LastActionAt: old},
		{ID: "nr", State: StateAwaiting, NeedsReview: true, PolicyType: "cgl", SubjectLine: "Review one", FromAddress: "n@x", CreatedAt: old, LastActionAt: old},
		{ID: "es", State: StateEscalated, PolicyType: "cgl", SubjectLine: "Escalated one", FromAddress: "e@x", CreatedAt: old, LastActionAt: old},
		{ID: "un", State: StateAwaiting, PolicyType: PolicyTypeUnknown, SubjectLine: "Unknown one", FromAddress: "u@x", CreatedAt: old, LastActionAt: old},
	}
	body := BuildDigest(subs, nil, 0, now)

	// a leading line counts the submissions needing a human
	if !strings.Contains(body, "1 submission(s) need review") {
		t.Errorf("missing review-count line:\n%s", body)
	}
	// sections appear in priority order: escalated, awaiting, complete, unknown
	titles := []string{"Escalated", "Awaiting the broker", "Complete", "Unknown policy type"}
	lastIdx := -1
	for _, title := range titles {
		idx := strings.Index(body, title)
		if idx < 0 {
			t.Fatalf("section %q missing:\n%s", title, body)
		}
		if idx < lastIdx {
			t.Errorf("section %q out of priority order:\n%s", title, body)
		}
		lastIdx = idx
	}
	// review is a flag, not a section: the flagged awaiting row is marked in place
	for _, want := range []string{
		"Escalated — broker went quiet (1)",
		"Awaiting the broker (2)", // plain + flagged awaiting
		"Complete (1)",
		"Unknown policy type (1)",
		"Review one | from n@x | age 1h | idle 1h [needs review]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
}

func TestBuildDigest_EmptyIsBlank(t *testing.T) {
	if got := BuildDigest(nil, nil, 0, time.Now()); got != "" {
		t.Errorf("an empty digest should render nothing, got %q", got)
	}
}

func TestBuildDigest_OmittedLine(t *testing.T) {
	now := time.Now()
	subs := []Submission{{ID: "a", State: StateAwaiting, SubjectLine: "S", FromAddress: "a@x", CreatedAt: now, LastActionAt: now}}
	if body := BuildDigest(subs, nil, 42, now); !strings.Contains(body, "42 more open submissions omitted") {
		t.Errorf("omitted line missing:\n%s", body)
	}
}

func TestBuildDigest_NoInternalItemIds(t *testing.T) {
	now := time.Now()
	subs := []Submission{{
		ID: "a", State: StateAwaiting, SubjectLine: "New submission", FromAddress: "a@x", CreatedAt: now, LastActionAt: now,
		MissingItems: []MissingItem{{ID: "acord_125", Description: "ACORD 125 Application", Reason: "document not provided", Code: ReasonNotProvided}},
	}}
	body := BuildDigest(subs, nil, 0, now)
	if strings.Contains(body, "acord_125") {
		t.Errorf("digest leaked an internal item id:\n%s", body)
	}
	if !strings.Contains(body, "ACORD 125 Application") {
		t.Errorf("digest should show the human item name:\n%s", body)
	}
}

func TestBuildDigest_FlagsFilenameOnlyInCompleteAndAwaiting(t *testing.T) {
	now := time.Now()
	subs := []Submission{
		{ID: "done", State: StateComplete, PolicyType: "cgl", SubjectLine: "Done", FromAddress: "d@x", CreatedAt: now, LastActionAt: now},
		{ID: "wait", State: StateAwaiting, PolicyType: "cgl", SubjectLine: "Wait", FromAddress: "w@x", CreatedAt: now, LastActionAt: now},
		{ID: "esc", State: StateEscalated, PolicyType: "cgl", SubjectLine: "Esc", FromAddress: "e@x", CreatedAt: now, LastActionAt: now},
	}
	filenameOnly := map[string][]string{
		"done": {"ACORD 125 Application"},
		"wait": {"Loss Runs"},
		"esc":  {"Should Not Appear"}, // escalated section is not flagged
	}
	body := BuildDigest(subs, filenameOnly, 0, now)

	const warn = "Matched on filename only, content not verified:"
	if !strings.Contains(body, warn+" ACORD 125 Application") {
		t.Errorf("complete section missing filename-only flag:\n%s", body)
	}
	if !strings.Contains(body, warn+" Loss Runs") {
		t.Errorf("awaiting section missing filename-only flag:\n%s", body)
	}
	if strings.Contains(body, "Should Not Appear") {
		t.Errorf("escalated section should not carry the filename-only flag:\n%s", body)
	}
}

func TestBuildDigest_NoFlagWhenCorroborated(t *testing.T) {
	now := time.Now()
	subs := []Submission{{ID: "done", State: StateComplete, PolicyType: "cgl", SubjectLine: "Done", FromAddress: "d@x", CreatedAt: now, LastActionAt: now}}
	// empty filenameOnly map: every match was corroborated by keyword/LLM
	body := BuildDigest(subs, map[string][]string{}, 0, now)
	if strings.Contains(body, "Matched on filename only") {
		t.Errorf("no flag expected when nothing matched by filename alone:\n%s", body)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second: "just now",
		45 * time.Minute: "45m",
		5 * time.Hour:    "5h",
		50 * time.Hour:   "2d",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
