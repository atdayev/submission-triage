package emailingest

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/atdayev/submission-triage/pkg/emlparse"
)

func samplePayload() emlparse.Payload {
	return emlparse.Payload{
		MessageID: "<root@x>",
		From:      "alice@example.com",
		FromFull:  emlparse.Address{Email: "alice@example.com", Name: "Alice"},
		To:        "submissions@triage.example",
		ToFull:    []emlparse.Address{{Email: "submissions@triage.example"}},
		Subject:   "New Submission - CGL",
		TextBody:  "hello",
		Date:      "Mon, 19 May 2026 09:00:00 -0400",
		Headers: []emlparse.Header{
			{Name: "In-Reply-To", Value: "<prev@x>"},
			{Name: "References", Value: "<root@x> <reply1@x>"},
		},
		Attachments: []emlparse.Attachment{
			{
				Name:        "doc.pdf",
				Content:     base64.StdEncoding.EncodeToString([]byte("hi")),
				ContentType: "application/pdf",
			},
		},
	}
}

func TestTranslate_StripsAngleBracketsFromMessageIDAndInReplyTo(t *testing.T) {
	r := Translate(samplePayload(), "imap")
	if r.MessageID != "root@x" {
		t.Errorf("MessageID: got %q", r.MessageID)
	}
	if r.InReplyTo != "prev@x" {
		t.Errorf("InReplyTo: got %q", r.InReplyTo)
	}
}

func TestTranslate_SplitsReferencesHeader(t *testing.T) {
	r := Translate(samplePayload(), "imap")
	if len(r.References) != 2 || r.References[0] != "root@x" || r.References[1] != "reply1@x" {
		t.Fatalf("References: got %+v", r.References)
	}
}

func TestTranslate_ReferencesSkipsGarbageTokens(t *testing.T) {
	cases := map[string][]string{
		"junk>more <real@x>": {"real@x"},
		"a>b<c@x>":           {"c@x"},
	}
	for in, want := range cases {
		p := samplePayload()
		p.Headers[1].Value = in
		r := Translate(p, "imap")
		if len(r.References) != len(want) {
			t.Fatalf("%q: got %+v, want %+v", in, r.References, want)
		}
		for i := range want {
			if r.References[i] != want[i] {
				t.Errorf("%q: got %+v, want %+v", in, r.References, want)
			}
		}
	}
}

func TestTranslate_ReferencesFallbackKeepsOnlyMessageIDs(t *testing.T) {
	p := samplePayload()
	p.Headers[1].Value = "garbage root@x notanid"
	r := Translate(p, "imap")
	if len(r.References) != 1 || r.References[0] != "root@x" {
		t.Fatalf("References: got %+v", r.References)
	}
}

func TestTranslate_PopulatesFromAndTo(t *testing.T) {
	r := Translate(samplePayload(), "imap")
	if r.FromAddress != "alice@example.com" || r.FromName != "Alice" {
		t.Errorf("From: %q %q", r.FromAddress, r.FromName)
	}
	if len(r.ToAddresses) != 1 || r.ToAddresses[0] != "submissions@triage.example" {
		t.Errorf("To: %+v", r.ToAddresses)
	}
}

func TestTranslate_FallsBackToFromAndToStringsWhenFullEmpty(t *testing.T) {
	p := samplePayload()
	p.FromFull = emlparse.Address{}
	p.ToFull = nil
	r := Translate(p, "imap")
	if r.FromAddress != "alice@example.com" {
		t.Errorf("FromAddress fallback: got %q", r.FromAddress)
	}
	if len(r.ToAddresses) != 1 || r.ToAddresses[0] != "submissions@triage.example" {
		t.Errorf("ToAddresses fallback: %+v", r.ToAddresses)
	}
}

func TestTranslate_DecodesAttachmentAndComputesSHA(t *testing.T) {
	r := Translate(samplePayload(), "imap")
	if len(r.Attachments) != 1 {
		t.Fatalf("attachments: got %d", len(r.Attachments))
	}
	a := r.Attachments[0]
	if a.Filename != "doc.pdf" || a.ContentType != "application/pdf" {
		t.Errorf("attachment meta: %+v", a)
	}
	if string(a.Content) != "hi" {
		t.Errorf("content: got %q", string(a.Content))
	}
	if a.Size != 2 {
		t.Errorf("size: got %d", a.Size)
	}
	if a.SHA256 == "" {
		t.Error("SHA256: empty")
	}
}

func TestTranslate_BadBase64AttachmentSkipped(t *testing.T) {
	p := samplePayload()
	p.Attachments[0].Content = "!!! not base64 !!!"
	r := Translate(p, "imap")
	if len(r.Attachments) != 0 {
		t.Fatalf("expected attachment skipped, got %d", len(r.Attachments))
	}
}

func TestTranslate_EmptyAttachmentSkipped(t *testing.T) {
	p := samplePayload()
	p.Attachments[0].Content = ""
	r := Translate(p, "imap")
	if len(r.Attachments) != 0 {
		t.Fatalf("expected attachment skipped, got %d", len(r.Attachments))
	}
}

func TestTranslate_NamedZeroOffsetZoneFallsBackToNow(t *testing.T) {
	p := samplePayload()
	p.Date = "Mon, 19 May 2026 09:00:00 EST"
	r := Translate(p, "imap")
	if r.ReceivedAt.Equal(time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)) {
		t.Fatal("named zone mis-stamped at +0000")
	}
	if r.ReceivedAt.IsZero() {
		t.Fatal("ReceivedAt should default to now")
	}
}

func TestTranslate_ParsesRFC1123ZDate(t *testing.T) {
	r := Translate(samplePayload(), "imap")
	want := time.Date(2026, 5, 19, 13, 0, 0, 0, time.UTC)
	if !r.ReceivedAt.Equal(want) {
		t.Errorf("ReceivedAt: got %v, want %v", r.ReceivedAt, want)
	}
}

func TestTranslate_InvalidDateFallsBackToNow(t *testing.T) {
	p := samplePayload()
	p.Date = "not-a-date"
	r := Translate(p, "imap")
	if r.ReceivedAt.IsZero() {
		t.Fatal("ReceivedAt should default to now when date unparseable")
	}
}

func TestTranslate_StampsSource(t *testing.T) {
	if got := Translate(samplePayload(), "imap").Source; got != "imap" {
		t.Errorf("Source: got %q, want imap", got)
	}
}

func TestTranslate_DetectsAutoSubmitted(t *testing.T) {
	cases := []struct {
		name   string
		hdr    emlparse.Header
		expect bool
	}{
		{"auto-replied", emlparse.Header{Name: "Auto-Submitted", Value: "auto-replied"}, true},
		{"auto-submitted-no", emlparse.Header{Name: "Auto-Submitted", Value: "no"}, false},
		{"precedence-bulk", emlparse.Header{Name: "Precedence", Value: "bulk"}, true},
		{"precedence-list", emlparse.Header{Name: "Precedence", Value: "list"}, true},
		{"list-id", emlparse.Header{Name: "List-Id", Value: "<news.example.com>"}, true},
		{"list-unsubscribe", emlparse.Header{Name: "List-Unsubscribe", Value: "<mailto:x@y>"}, true},
		{"null-return-path", emlparse.Header{Name: "Return-Path", Value: "<>"}, true},
		{"normal-return-path", emlparse.Header{Name: "Return-Path", Value: "<broker@x>"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := samplePayload()
			p.Headers = append(p.Headers, tc.hdr)
			r := Translate(p, "imap")
			if r.AutoSubmitted != tc.expect {
				t.Errorf("AutoSubmitted: got %v, want %v", r.AutoSubmitted, tc.expect)
			}
			if tc.expect && r.AutoResponseHeaders == nil {
				t.Error("AutoResponseHeaders should be populated when auto-submitted")
			}
		})
	}
}

func TestTranslate_NormalMailNotAutoSubmitted(t *testing.T) {
	r := Translate(samplePayload(), "imap")
	if r.AutoSubmitted {
		t.Error("a plain broker email must not be flagged auto-submitted")
	}
	if r.AutoResponseHeaders != nil {
		t.Errorf("no auto-response headers expected, got %v", r.AutoResponseHeaders)
	}
}
