package emlparse

import (
	"encoding/base64"
	"strings"
	"testing"
)

// base64 with an interior space must still decode to the real bytes.
func TestFromReader_Base64WithInteriorSpaceDecodes(t *testing.T) {
	const raw = "From: a@x\r\nTo: b@y\r\nSubject: t\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"bb\"\r\n\r\n" +
		"--bb\r\nContent-Type: application/pdf; name=\"f.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"f.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"SGVsbG8g UERG\r\n--bb--\r\n" // base64("Hello PDF") with a space inserted

	p, err := FromReader(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments: got %d, want 1", len(p.Attachments))
	}
	got, err := base64.StdEncoding.DecodeString(p.Attachments[0].Content)
	if err != nil {
		t.Fatalf("attachment content is not valid base64: %v", err)
	}
	if string(got) != "Hello PDF" {
		t.Errorf("attachment decoded to %q, want %q (double-encoded?)", got, "Hello PDF")
	}
}

// A multipart message whose only text part is text/html must not lose the body.
func TestFromReader_HTMLOnlyMultipartFallsBack(t *testing.T) {
	const raw = "From: a@x\r\nTo: b@y\r\nSubject: t\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"bb\"\r\n\r\n" +
		"--bb\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>We need the <b>loss&nbsp;run</b>.</p>\r\n--bb--\r\n"

	p, err := FromReader(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if !strings.Contains(strings.ToLower(p.TextBody), "loss") {
		t.Errorf("HTML-only body lost: TextBody=%q", p.TextBody)
	}
	if strings.Contains(p.TextBody, "<p>") {
		t.Errorf("HTML tags not stripped: TextBody=%q", p.TextBody)
	}
}
