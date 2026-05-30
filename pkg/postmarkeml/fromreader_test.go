package postmarkeml

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
