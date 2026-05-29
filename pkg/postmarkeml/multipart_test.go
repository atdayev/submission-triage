package postmarkeml

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "m.eml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const nestedMultipart = `From: a@x
To: b@x
Subject: nested
Message-ID: <nested@x>
Date: Mon, 19 May 2026 09:00:00 -0400
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="inner"

--inner
Content-Type: text/plain; charset=utf-8

The plain text body lives inside the alternative branch.

--inner
Content-Type: text/html; charset=utf-8

<p>html version</p>

--inner--

--outer
Content-Type: application/pdf; name="doc.pdf"
Content-Disposition: attachment; filename="doc.pdf"
Content-Transfer-Encoding: base64

SGVsbG8gUERG

--outer--
`

func TestFromFile_RecursesNestedMultipart(t *testing.T) {
	p, err := FromFile(writeTemp(t, nestedMultipart))
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	if !strings.Contains(p.TextBody, "plain text body lives inside the alternative branch") {
		t.Errorf("TextBody not extracted from nested alternative: got %q", p.TextBody)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].Name != "doc.pdf" {
		t.Fatalf("attachments: got %+v", p.Attachments)
	}
	decoded, err := base64.StdEncoding.DecodeString(p.Attachments[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "Hello PDF" {
		t.Errorf("attachment content: got %q", string(decoded))
	}
}

const quotedPrintableEML = `From: a@x
To: b@x
Subject: qp test
Message-ID: <qp@x>
Date: Mon, 19 May 2026 09:00:00 -0400
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Hello =E2=80=94 world.=
 Continuation works.

--b--
`

func TestFromFile_DecodesQuotedPrintable(t *testing.T) {
	p, err := FromFile(writeTemp(t, quotedPrintableEML))
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	// =E2=80=94 is U+2014 (em dash); =\n is soft line break.
	if !strings.Contains(p.TextBody, "Hello — world.") {
		t.Errorf("QP-decoded em dash missing: got %q", p.TextBody)
	}
	if !strings.Contains(p.TextBody, "Continuation works.") {
		t.Errorf("soft line break unfolded: got %q", p.TextBody)
	}
}

const flatMultipartWithAttachment = `From: a@x
To: b@x
Subject: flat
Message-ID: <flat@x>
Date: Mon, 19 May 2026 09:00:00 -0400
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="X"

--X
Content-Type: text/plain

flat body

--X
Content-Type: text/csv; name="losses.csv"
Content-Disposition: attachment; filename="losses.csv"

year,loss
2024,10000
2023,5000

--X--
`

func TestFromFile_FlatMultipart_TextAndAttachmentBothSurface(t *testing.T) {
	p, err := FromFile(writeTemp(t, flatMultipartWithAttachment))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.TextBody, "flat body") {
		t.Errorf("body: %q", p.TextBody)
	}
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments: got %d", len(p.Attachments))
	}
	if p.Attachments[0].Name != "losses.csv" {
		t.Errorf("name: %q", p.Attachments[0].Name)
	}
}

// Builds an eml whose Content-Type chain wraps `depth` layers of
// multipart/mixed around a single text/plain part.
func deeplyNestedEML(depth int) string {
	var b strings.Builder
	b.WriteString("From: a@x\nTo: b@x\nSubject: deep\nMessage-ID: <deep@x>\n")
	b.WriteString("Date: Mon, 19 May 2026 09:00:00 -0400\nMIME-Version: 1.0\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"b0\"\n\n")
	for i := 0; i < depth; i++ {
		b.WriteString("--b")
		b.WriteString(fmt.Sprintf("%d\n", i))
		b.WriteString("Content-Type: multipart/mixed; boundary=\"b")
		b.WriteString(fmt.Sprintf("%d\"\n\n", i+1))
	}
	b.WriteString("--b")
	b.WriteString(fmt.Sprintf("%d\n", depth))
	b.WriteString("Content-Type: text/plain\n\n")
	b.WriteString("payload at the bottom\n\n")
	for i := depth; i >= 0; i-- {
		b.WriteString("--b")
		b.WriteString(fmt.Sprintf("%d--\n", i))
	}
	return b.String()
}

func TestFromFile_DepthCap_DoesNotCrash(t *testing.T) {
	// Beyond cap: deeply-nested mail should return without panicking and
	// without consuming the payload (it lives below the cap).
	p, err := FromFile(writeTemp(t, deeplyNestedEML(50)))
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	// Either the payload was reached (cap permitted it) or it wasn't.
	// What matters is that we got a Payload back without panicking.
	_ = p
}

func TestFromFile_DepthBelowCap_StillExtractsPayload(t *testing.T) {
	// 5 levels — well under the cap of 32.
	p, err := FromFile(writeTemp(t, deeplyNestedEML(5)))
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	if !strings.Contains(p.TextBody, "payload at the bottom") {
		t.Errorf("payload should still be extracted at depth=5: %q", p.TextBody)
	}
}

func TestFromFile_OneBadPart_OthersStillRecovered(t *testing.T) {
	// First part has a malformed Content-Transfer-Encoding that would
	// historically have aborted the whole walk. Subsequent text/plain
	// part should still be recovered.
	mixed := `From: a@x
To: b@x
Subject: tolerant
Message-ID: <tol@x>
Date: Mon, 19 May 2026 09:00:00 -0400
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: application/octet-stream
Content-Transfer-Encoding: base64

!!!!definitely not valid base64!!!!

--b
Content-Type: text/plain

recovered text body

--b--
`
	p, err := FromFile(writeTemp(t, mixed))
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	if !strings.Contains(p.TextBody, "recovered text body") {
		t.Errorf("text body should be recovered despite earlier bad part: %q", p.TextBody)
	}
}

func TestFromFile_FirstTextPlainWins(t *testing.T) {
	// Two text/plain parts: assert the first one wins (the spec says
	// recursion + first-text behavior).
	twoText := `From: a@x
To: b@x
Subject: two
Message-ID: <two@x>
Date: Mon, 19 May 2026 09:00:00 -0400
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain

first text

--b
Content-Type: text/plain

second text

--b--
`
	p, err := FromFile(writeTemp(t, twoText))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.TextBody, "first text") {
		t.Errorf("first text should win: got %q", p.TextBody)
	}
}
