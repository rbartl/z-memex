package memory

import (
	"regexp"
	"strings"
)

// A trailing parenthetical is a gloss on the name, not part of it: the corpus
// carried "Go" beside "Go (Golang)" and "TLS" beside "TLS (Transport Layer
// Team)". Only a trailing group is stripped, and only when something survives —
// a name that is nothing but a parenthetical keeps it.
var trailingParenthetical = regexp.MustCompile(`\s*\([^()]*\)\s*$`)

// Hyphens, underscores and slashes are separator spellings of a space:
// "acme-invoice-sync" and "Acme Invoice Sync" are one project.
// Other punctuation is left alone on purpose — folding it would collapse "C++"
// and "C#" onto "c".
var nameSeparators = regexp.MustCompile(`[-_/]+`)

// NormalizeName folds an entity name to the key used for identity. Entity names
// arrive from an LLM, which spells the same thing differently from call to call:
// it varies the case ("Acme"/"ACME"), appends a gloss ("Go (Golang)"),
// swaps separators ("acme-invoice-sync"), and quotes the odd name
// ("'Sprint Review'"). All of those describe one node. The original spelling is
// kept alongside for display; only this form is unique.
//
// Migrations 007 and 009 backfill the same expression in SQL, so the two must
// agree — 009 carries the SQL twin of every rule below.
func NormalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))

	if stripped := trailingParenthetical.ReplaceAllString(s, ""); strings.TrimSpace(stripped) != "" {
		s = stripped
	}

	s = nameSeparators.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")

	// Quotes survive the steps above because they sit at the edges of the name
	// rather than between words.
	return strings.Trim(s, "'\"`")
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
