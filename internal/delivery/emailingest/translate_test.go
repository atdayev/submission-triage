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
