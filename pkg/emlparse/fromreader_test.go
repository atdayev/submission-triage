package emlparse

import (
	"strings"
	"testing"
)

func TestFromReader_ParsesRawMessage(t *testing.T) {
	p, err := FromReader(strings.NewReader(simpleEML))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if p.MessageID != "abc-123@example.com" {
		t.Errorf("MessageID: got %q", p.MessageID)
	}
	if !strings.Contains(p.TextBody, "Hello, world.") {
		t.Errorf("TextBody: got %q", p.TextBody)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].Name != "doc.pdf" {
		t.Errorf("attachments: got %+v", p.Attachments)
	}
}

func TestFromReader_MatchesFromFile(t *testing.T) {
	path := writeTempEML(t, simpleEML)
	fromFile, err := FromFile(path)
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	fromReader, err := FromReader(strings.NewReader(simpleEML))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if fromFile.MessageID != fromReader.MessageID || fromFile.TextBody != fromReader.TextBody {
		t.Errorf("FromFile and FromReader disagree:\nfile=%+v\nreader=%+v", fromFile, fromReader)
	}
}

func TestFromReader_InvalidReturnsError(t *testing.T) {
	if _, err := FromReader(strings.NewReader("this is not an email")); err == nil {
		t.Fatal("expected error for malformed message")
	}
}

func TestFromReader_DecodesRFC2047Subject(t *testing.T) {
	cases := map[string]string{
		"=?UTF-8?Q?Caf=C3=A9_r=C3=A9sum=C3=A9?=": "Café résumé", // Q-encoded
		"=?UTF-8?B?5L+d6Zmp?=":                   "保险",          // B-encoded (Chinese)
		"Plain ASCII Submission":                 "Plain ASCII Submission",
	}
	for raw, want := range cases {
		t.Run(want, func(t *testing.T) {
			eml := "From: a@b.com\r\nTo: c@d.com\r\nSubject: " + raw + "\r\n\r\nbody\r\n"
			p, err := FromReader(strings.NewReader(eml))
			if err != nil {
				t.Fatalf("FromReader: %v", err)
			}
			if p.Subject != want {
				t.Errorf("subject: got %q, want %q", p.Subject, want)
			}
		})
	}
}

func TestFromReader_UndecodableSubjectPassesThrough(t *testing.T) {
	// unknown charset -> decoder errors -> keep the raw header rather than drop it
	eml := "From: a@b.com\r\nTo: c@d.com\r\nSubject: =?x-unknown?Q?abc?=\r\n\r\nbody\r\n"
	p, err := FromReader(strings.NewReader(eml))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if p.Subject != "=?x-unknown?Q?abc?=" {
		t.Errorf("subject: got %q, want raw passthrough", p.Subject)
	}
}
