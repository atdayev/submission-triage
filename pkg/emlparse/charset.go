package emlparse

import "strings"

// toUTF8 transcodes the charsets we handle; others pass through unchanged.
func toUTF8(b []byte, charset string) string {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "iso-8859-1", "iso8859-1", "latin-1", "latin1":
		return decodeLatin1(b)
	case "windows-1252", "cp1252":
		return decodeWindows1252(b)
	default:
		return string(b)
	}
}

func decodeLatin1(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

// cp1252High is Windows-1252's 0x80–0x9F block, where it diverges from ISO-8859-1.
var cp1252High = [...]rune{
	0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…', 0x86: '†', 0x87: '‡',
	0x88: 'ˆ', 0x89: '‰', 0x8A: 'Š', 0x8B: '‹', 0x8C: 'Œ', 0x8E: 'Ž',
	0x91: '‘', 0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—',
	0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›', 0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ',
}

func decodeWindows1252(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c >= 0x80 && c <= 0x9F && cp1252High[c] != 0 {
			sb.WriteRune(cp1252High[c])
			continue
		}
		sb.WriteRune(rune(c))
	}
	return sb.String()
}
