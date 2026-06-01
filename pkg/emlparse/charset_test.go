package emlparse

import (
	"strings"
	"testing"
)

func emlWithBody(charset string, body []byte) string {
	return "From: a@b.com\r\nTo: c@d.com\r\nSubject: x\r\n" +
		"Content-Type: text/plain; charset=" + charset + "\r\n\r\n" + string(body)
}

func TestFromReader_TranscodesISO88591Body(t *testing.T) {
	// "Café" with é as the single Latin-1 byte 0xE9
	body := append([]byte("Caf"), 0xE9)
	p, err := FromReader(strings.NewReader(emlWithBody("ISO-8859-1", body)))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if !strings.Contains(p.TextBody, "Café") {
		t.Errorf("body not transcoded from latin-1: got %q", p.TextBody)
	}
}

func TestFromReader_TranscodesWindows1252Body(t *testing.T) {
	// 0x92 -> right single quote (’), 0x80 -> euro (€): both differ from latin-1
	body := append([]byte("don"), 0x92)
	body = append(body, 't', ' ', 0x80, '5')
	p, err := FromReader(strings.NewReader(emlWithBody("windows-1252", body)))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if !strings.Contains(p.TextBody, "don’t") {
		t.Errorf("smart quote not transcoded: got %q", p.TextBody)
	}
	if !strings.Contains(p.TextBody, "€5") {
		t.Errorf("euro sign not transcoded: got %q", p.TextBody)
	}
}

func TestFromReader_UTF8BodyUnchanged(t *testing.T) {
	p, err := FromReader(strings.NewReader(emlWithBody("utf-8", []byte("Café €5"))))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if !strings.Contains(p.TextBody, "Café €5") {
		t.Errorf("utf-8 body altered: got %q", p.TextBody)
	}
}

func TestFromReader_UnknownCharsetPassesThrough(t *testing.T) {
	// we don't transcode shift_jis; bytes survive verbatim rather than being dropped
	body := append([]byte("data"), 0xE9)
	p, err := FromReader(strings.NewReader(emlWithBody("shift_jis", body)))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if !strings.Contains(p.TextBody, string([]byte{0xE9})) {
		t.Errorf("unknown charset bytes not preserved: got %q", p.TextBody)
	}
}

func TestFromReader_QuotedPrintableThenCharset(t *testing.T) {
	// quoted-printable AND windows-1252: =92/=80 decode to bytes, then transcode
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: x\r\n" +
		"Content-Type: text/plain; charset=windows-1252\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"don=92t pay =80100\r\n"
	p, err := FromReader(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if !strings.Contains(p.TextBody, "don’t pay €100") {
		t.Errorf("QP+charset chain wrong: got %q", p.TextBody)
	}
}

func TestFromReader_DecodesRFC2047AttachmentName(t *testing.T) {
	raw := "From: a@b.com\r\nTo: c@d.com\r\nSubject: x\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--B\r\nContent-Type: application/pdf; name=\"=?UTF-8?Q?r=C3=A9serv=C3=A9.pdf?=\"\r\n" +
		"Content-Disposition: attachment\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\naGk=\r\n" +
		"--B--\r\n"
	p, err := FromReader(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments: got %d, want 1", len(p.Attachments))
	}
	if p.Attachments[0].Name != "réservé.pdf" {
		t.Errorf("filename not decoded: got %q", p.Attachments[0].Name)
	}
}
