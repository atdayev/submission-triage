package textutil

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateBytes_NoSplit(t *testing.T) {
	// "héllo": é is 2 bytes (0xC3 0xA9); a byte-2 cut must back up, not split it
	if got := TruncateBytes("héllo", 2); got != "h" || !utf8.ValidString(got) {
		t.Errorf("TruncateBytes(\"héllo\", 2) = %q (valid=%v); want \"h\"", got, utf8.ValidString(got))
	}
	if got := TruncateBytes("héllo", 3); got != "hé" {
		t.Errorf("TruncateBytes(\"héllo\", 3) = %q; want \"hé\"", got)
	}
	if got := TruncateBytes("abc", 10); got != "abc" {
		t.Errorf("no truncation expected: got %q", got)
	}
}
