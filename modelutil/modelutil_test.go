package modelutil

import "testing"

func TestSanitizeModel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Trailing bracket suffixes — stripped
		{"lowercase 1m", "deepseek-v4-flash[1m]", "deepseek-v4-flash"},
		{"uppercase 1M", "deepseek-v4-flash[1M]", "deepseek-v4-flash"},
		{"1K suffix", "deepseek-v4-flash[1K]", "deepseek-v4-flash"},
		{"1h suffix", "deepseek-chat[1h]", "deepseek-chat"},
		{"arbitrary content", "model[anything]", "model"},
		{"empty brackets", "model[]", "model"},

		// No trailing bracket — returned as-is
		{"no suffix", "deepseek-v4-flash", "deepseek-v4-flash"},
		{"empty string", "", ""},
		{"plain name", "gpt-4o", "gpt-4o"},

		// Middle brackets — NOT stripped (only trailing [....] is removed)
		{"middle brackets", "model[foo]bar", "model[foo]bar"},
		{"bracket not at end", "model[1m]extra", "model[1m]extra"},

		// Only the LAST trailing bracket group is stripped
		{"double trailing", "model[1m][2m]", "model[1m]"},

		// Unmatched brackets — returned as-is
		{"unmatched open", "model[1m", "model[1m"},
		{"unmatched close", "model1m]", "model1m]"},

		// Edge cases
		{"only brackets", "[]", ""},
		{"bracket at start", "[1m]model", "[1m]model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizeModel(c.in)
			if got != c.want {
				t.Errorf("SanitizeModel(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
