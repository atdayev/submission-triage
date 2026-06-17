package glob

import (
	"path"
	"path/filepath"
	"strings"
)

// MatchAny reports whether target's base name matches any glob pattern, case-insensitively.
func MatchAny(patterns []string, target string) bool {
	lower := strings.ToLower(path.Base(target))
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		ok, err := filepath.Match(strings.ToLower(pattern), lower)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// ContainsAny reports whether body contains any keyword, case-insensitively.
func ContainsAny(keywords []string, body string) bool {
	if body == "" {
		return false
	}
	lower := strings.ToLower(body)
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
