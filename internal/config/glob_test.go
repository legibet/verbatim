package config

import "testing"

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"kimi-k3", "kimi-k3", true},
		{"kimi-k3", "kimi-k3-0905", false},
		{"kimi-k3", "Kimi-k3", false},
		{"*", "", true},
		{"*", "anything/at/all", true},
		{"gpt-*", "gpt-5", true},
		{"gpt-*", "gpt-", true},
		{"gpt-*", "GPT-5", false},
		{"gpt-*", "xgpt-5", false},
		{"anthropic/*", "anthropic/claude-opus-5-5", true},
		{"*/claude-*", "anthropic/claude-opus-5-5", true},
		{"*-5-5", "claude-opus-5-5", true},
		{"*-5-5", "claude-opus-5-5x", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "aXbYbZc", true},
		{"a*b*c", "acb", false},
		{"ab*ba", "aba", false}, // prefix and suffix must not overlap
		{"a**b", "ab", true},
		{"model?", "model?", true},
		{"model?", "models", false},
		{"[ab]", "[ab]", true},
		{"[ab]", "a", false},
		{`a\*`, `a\xyz`, true},
	}
	for _, tt := range tests {
		if got := MatchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestIsExact(t *testing.T) {
	if !IsExact("kimi-k3") || !IsExact("model?") || IsExact("gpt-*") || IsExact("*") {
		t.Error("IsExact misclassifies patterns")
	}
}
