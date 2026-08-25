package memory

import "strings"

// NormalizeName folds an entity name to the key used for identity. Entity names
// arrive from an LLM, which capitalises inconsistently across calls, so "Acme"
// and "ACME" describe one organisation and must resolve to one node. The
// original spelling is kept alongside for display; only this form is unique.
//
// Migration 007 backfills the same expression in SQL, so the two must agree.
func NormalizeName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// NormalizeTags applies NormalizeName to every tag, dropping empties and
// duplicates while preserving first-seen order. Tags are free text chosen by
// whichever client stored the memory, and the same disease shows up there:
// "acme" and "ACME" were separate tags on separate memories.
func NormalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		n := NormalizeName(t)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
