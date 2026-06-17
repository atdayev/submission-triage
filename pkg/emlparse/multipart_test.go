package emlparse

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
	// beyond cap: deeply-nested mail must return without panicking
	p, err := FromFile(writeTemp(t, deeplyNestedEML(50)))
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
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
	// first part has a malformed CTE; the later text/plain part must still be recovered
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

// flatManyAttachments builds a flat multipart with one text body and n named
// attachment parts.
func flatManyAttachments(n int) string {
	var b strings.Builder
	b.WriteString("From: a@x\nTo: b@x\nSubject: many\nMessage-ID: <many@x>\n")
	b.WriteString("Date: Mon, 19 May 2026 09:00:00 -0400\nMIME-Version: 1.0\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"b\"\n\n")
	b.WriteString("--b\nContent-Type: text/plain\n\nflat body\n\n")
	for i := 0; i < n; i++ {
		b.WriteString(fmt.Sprintf("--b\nContent-Type: application/octet-stream; name=\"a%d.bin\"\n", i))
		b.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"a%d.bin\"\n", i))
		b.WriteString("Content-Transfer-Encoding: base64\n\nAAAA\n\n")
	}
	b.WriteString("--b--\n")
	return b.String()
}

func TestFromFile_AttachmentCap_BoundedAndBodySurvives(t *testing.T) {
	// 150 attachment parts: the cap bounds the slice at 100 and the body still surfaces
	p, err := FromFile(writeTemp(t, flatManyAttachments(150)))
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	if len(p.Attachments) != 100 {
		t.Errorf("attachment cap: got %d, want 100", len(p.Attachments))
	}
	if !strings.Contains(p.TextBody, "flat body") {
		t.Errorf("text body should still surface under the cap: %q", p.TextBody)
	}
}

func TestFromFile_FirstTextPlainWins(t *testing.T) {
	// two text/plain parts: the first one wins
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
