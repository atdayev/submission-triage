package emlparse

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

const simpleEML = `From: Alice <alice@example.com>
To: Bob <bob@example.com>
Subject: Hello
Message-ID: <abc-123@example.com>
Date: Mon, 19 May 2026 09:00:00 -0400
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="bnd"

--bnd
Content-Type: text/plain; charset=utf-8

Hello, world.

--bnd
Content-Type: application/pdf; name="doc.pdf"
Content-Disposition: attachment; filename="doc.pdf"
Content-Transfer-Encoding: base64

SGVsbG8gUERG

--bnd--
`

func writeTempEML(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "msg.eml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFromFile_ParsesHeadersBodyAndAttachment(t *testing.T) {
	path := writeTempEML(t, simpleEML)
	p, err := FromFile(path)
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}

	if p.MessageID != "abc-123@example.com" {
		t.Errorf("MessageID: got %q", p.MessageID)
	}
	if p.Subject != "Hello" {
		t.Errorf("Subject: got %q", p.Subject)
	}
	if p.From != "alice@example.com" || p.FromFull.Name != "Alice" {
		t.Errorf("From: got %+v", p.FromFull)
	}
	if len(p.ToFull) != 1 || p.ToFull[0].Email != "bob@example.com" {
		t.Errorf("To: got %+v", p.ToFull)
	}
	if p.TextBody != "Hello, world.\n" {
		t.Errorf("TextBody: got %q", p.TextBody)
	}
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments: got %d", len(p.Attachments))
	}
	a := p.Attachments[0]
	if a.Name != "doc.pdf" || a.ContentType != "application/pdf" {
		t.Errorf("attachment meta: got %+v", a)
	}
	decoded, err := base64.StdEncoding.DecodeString(a.Content)
	if err != nil {
		t.Fatalf("decode attachment content: %v", err)
	}
	if string(decoded) != "Hello PDF" {
		t.Errorf("attachment content: got %q", string(decoded))
	}
}

func TestFromFile_MissingFileReturnsError(t *testing.T) {
	_, err := FromFile(filepath.Join(t.TempDir(), "does-not-exist.eml"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFromFile_DefaultsDateWhenAbsent(t *testing.T) {
	const noDate = `From: a@b
To: c@d
Subject: x
Message-ID: <m@x>
MIME-Version: 1.0
Content-Type: text/plain

body
`
	p, err := FromFile(writeTempEML(t, noDate))
	if err != nil {
		t.Fatal(err)
	}
	if p.Date == "" {
		t.Fatal("expected Date to default to current time")
	}
}

func TestFromFile_ExtractsInReplyToAndReferences(t *testing.T) {
	const threaded = `From: a@b
To: c@d
Subject: Re: thread
Message-ID: <follow-up@x>
In-Reply-To: <root@x>
References: <root@x> <reply1@x>
Date: Mon, 19 May 2026 09:00:00 -0400
MIME-Version: 1.0
Content-Type: text/plain

body
`
	p, err := FromFile(writeTempEML(t, threaded))
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{}
	for _, h := range p.Headers {
		headers[h.Name] = h.Value
	}
	if headers["In-Reply-To"] != "root@x" {
		t.Errorf("In-Reply-To: got %q", headers["In-Reply-To"])
	}
	if headers["References"] != "<root@x> <reply1@x>" {
		t.Errorf("References: got %q", headers["References"])
	}
}
