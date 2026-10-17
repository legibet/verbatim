package config

import "strings"

// MatchGlob reports whether name matches pattern. '*' matches any run of
// characters, including '/', so "anthropic/*" matches OpenRouter-style names;
// every other character is literal. Matching is case-sensitive and anchored at
// both ends. path.Match is deliberately not used because its '*' stops at '/'
// and it gives '?', '[' and '\' special meaning.
func MatchGlob(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	first, last := parts[0], parts[len(parts)-1]
	if len(name) < len(first)+len(last) || !strings.HasPrefix(name, first) || !strings.HasSuffix(name, last) {
		return false
	}
	// With only '*' as a wildcard, taking the leftmost occurrence of each
	// middle part never loses a match.
	rest := name[len(first) : len(name)-len(last)]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, part)
		if i < 0 {
			return false
		}
		rest = rest[i+len(part):]
	}
	return true
}

// Capture returns the text that the single '*' of pattern matches in name.
// pattern must contain exactly one '*'.
func Capture(pattern, name string) (string, bool) {
	before, after, _ := strings.Cut(pattern, "*")
	if len(name) < len(before)+len(after) || !strings.HasPrefix(name, before) || !strings.HasSuffix(name, after) {
		return "", false
	}
	return name[len(before) : len(name)-len(after)], true
}

// IsExact reports whether a pattern contains no wildcard.
func IsExact(pattern string) bool {
	return !strings.Contains(pattern, "*")
}
