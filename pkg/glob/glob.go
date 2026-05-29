package glob

import (
	"path/filepath"
	"strings"
)

func MatchAny(patterns []string, target string) bool {
	lower := strings.ToLower(target)
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
