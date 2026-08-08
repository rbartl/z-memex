package chat

import "testing"

// The reported failure: the model answered "NONE" for "what bike do i ride?",
// the empty result skipped retrieval, and the model then answered from an empty
// context even though the memory existed and ranked first. An unusable rewrite
// must degrade to the user's own words, never to no query at all.
func TestCleanQuery(t *testing.T) {
	const fallback = "what bike do i ride?"

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"a usable rewrite is kept", "bike model user rides", "bike model user rides"},
		{"none falls back", "NONE", fallback},
		{"none is case insensitive", "none", fallback},
		{"none with whitespace falls back", "  None \n", fallback},
		{"empty falls back", "", fallback},
		{"whitespace only falls back", "   \n\t ", fallback},
		{"surrounding double quotes stripped", `"bike model user rides"`, "bike model user rides"},
		{"surrounding single quotes stripped", `'bike model'`, "bike model"},
		{"leading blank lines skipped", "\n\n  bike model user rides", "bike model user rides"},
		{"trailing commentary dropped", "bike model user rides\n\nThis query targets the bike.", "bike model user rides"},
		{"quoted none still falls back", `"none"`, fallback},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanQuery(tc.raw, fallback); got != tc.want {
				t.Errorf("cleanQuery(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// A model that ignores the instruction and writes an essay produces a worse
// search query than the user's own message.
func TestCleanQueryRejectsProse(t *testing.T) {
	const fallback = "what bike do i ride?"

	prose := make([]byte, maxSynthesizedQueryLen+1)
	for i := range prose {
		prose[i] = 'a'
	}

	if got := cleanQuery(string(prose), fallback); got != fallback {
		t.Errorf("over-long query was not rejected: got %q", got)
	}

	// A query right at the limit is still fine.
	atLimit := string(prose[:maxSynthesizedQueryLen])
	if got := cleanQuery(atLimit, fallback); got != atLimit {
		t.Errorf("query at the length limit was rejected: got %q", got)
	}
}

// cleanQuery must never return an empty string — that is what skipped
// retrieval in the first place.
func TestCleanQueryNeverReturnsEmpty(t *testing.T) {
	for _, raw := range []string{"", "   ", "NONE", "\n\n", `""`, "none\nnone"} {
		if got := cleanQuery(raw, "fallback"); got == "" {
			t.Errorf("cleanQuery(%q) returned an empty query", raw)
		}
	}
}
