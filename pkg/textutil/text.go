package textutil

import "unicode/utf8"

// TruncateBytes caps s to maxBytes without splitting a multi-byte rune.
func TruncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}
