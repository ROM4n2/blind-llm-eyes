// Package modelutil provides utilities for normalizing model names.
//
// The primary use case is stripping vendor-specific context-length suffixes
// (e.g. DeepSeek's [1m] / [1M] markers) that Claude Code or cc-switch may
// append to model names. These suffixes are meaningful to the vendor's own
// routing layer but must not be forwarded to upstream Anthropic-compatible
// endpoints, which would reject the unknown model name.
package modelutil

import "strings"

// SanitizeModel strips a single trailing "[...]" bracket group from the model
// name. It only removes the last bracket group if it is at the very end of the
// string; middle brackets are left untouched.
//
// Examples:
//
//	SanitizeModel("deepseek-v4-flash[1m]") → "deepseek-v4-flash"
//	SanitizeModel("deepseek-v4-flash[1M]") → "deepseek-v4-flash"
//	SanitizeModel("deepseek-v4-flash")     → "deepseek-v4-flash"
//	SanitizeModel("model[foo]bar")         → "model[foo]bar"
//	SanitizeModel("model[1m][2m]")         → "model[1m]"
//	SanitizeModel("")                      → ""
func SanitizeModel(m string) string {
	if len(m) < 2 || !strings.HasSuffix(m, "]") {
		return m
	}
	// Find the last "[" — everything from there to the end is a candidate
	// trailing bracket group.
	lastOpen := strings.LastIndex(m, "[")
	if lastOpen < 0 {
		return m
	}
	inner := m[lastOpen+1 : len(m)-1]
	// If the inner content contains "[" or "]", this is a nested/malformed
	// bracket group — leave it untouched.
	if strings.ContainsAny(inner, "[]") {
		return m
	}
	return m[:lastOpen]
}
