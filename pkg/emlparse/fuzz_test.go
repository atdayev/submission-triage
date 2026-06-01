package emlparse

import (
	"strings"
	"testing"
)

// FuzzFromReader feeds arbitrary bytes to the parser; it must never panic on
// untrusted input (malformed MIME, bad encodings, nesting, etc.).
func FuzzFromReader(f *testing.F) {
	seeds := []string{
		"",
		"not an email",
		"Subject: hi\r\nFrom: a@b.com\r\nTo: c@d.com\r\n\r\nbody",
		"Subject: =?UTF-8?Q?Caf=C3=A9?=\r\nFrom: a@b.com\r\n\r\nx",
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n" +
			"Content-Type: text/plain; charset=windows-1252\r\n\r\ndon=92t\r\n--B--\r\n",
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n" +
			"Content-Disposition: attachment\r\nContent-Transfer-Encoding: base64\r\n\r\n!!bad!!\r\n--B--\r\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = FromReader(strings.NewReader(raw))
	})
}
