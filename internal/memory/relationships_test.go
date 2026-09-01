package memory

import "testing"

func TestCanonicalRelationshipFoldsSpelling(t *testing.T) {
	// The three spellings that sat on one pair of nodes as three edges.
	for _, in := range []string{"holds", "holds role", "holds-role", "HOLDS_ROLE"} {
		label, reversed, ok := CanonicalRelationship(in)
		if !ok {
			t.Errorf("%q not recognized", in)
		}
		if label != "has-role" || reversed {
			t.Errorf("%q = (%q, reversed=%v), want (\"has-role\", false)", in, label, reversed)
		}
	}
}

// An inverse alias must come back reversed, or the same fact is stored twice —
// once as "Acme employs Max" and once as "Max works-at Acme".
func TestCanonicalRelationshipReversesInverses(t *testing.T) {
	cases := map[string]string{
		"employs":     "works-at",
		"used-by":     "uses",
		"led-by":      "leads",
		"created-by":  "creates",
		"owned-by":    "owns",
		"hosts":       "hosted-on",
		"reports-to":  "leads",
		"replaced-by": "supersedes",
	}
	for in, want := range cases {
		label, reversed, ok := CanonicalRelationship(in)
		if !ok || label != want || !reversed {
			t.Errorf("%q = (%q, reversed=%v, ok=%v), want (%q, true, true)", in, label, reversed, ok, want)
		}
	}
}

func TestCanonicalRelationshipKeepsVocabularyAsIs(t *testing.T) {
	for _, r := range Relationships {
		label, reversed, ok := CanonicalRelationship(r)
		if !ok || label != r || reversed {
			t.Errorf("%q = (%q, %v, %v), want it returned unchanged", r, label, reversed, ok)
		}
	}
}

func TestCanonicalRelationshipFallsBack(t *testing.T) {
	for _, in := range []string{"", "   ", "hypothesizes moving-to", "replicates-caps"} {
		label, reversed, ok := CanonicalRelationship(in)
		if ok {
			t.Errorf("%q reported as recognized", in)
		}
		if label != FallbackRelationship || reversed {
			t.Errorf("%q = (%q, %v), want (%q, false)", in, label, reversed, FallbackRelationship)
		}
	}
}

// Every alias must land on a real predicate, or writes fail the CHECK
// constraint migration 010 adds.
func TestEveryAliasTargetIsInVocabulary(t *testing.T) {
	for alias, c := range relationshipAliases {
		if !relationshipSet[c.label] {
			t.Errorf("alias %q maps to %q, which is not in Relationships", alias, c.label)
		}
		if relationshipSet[alias] {
			t.Errorf("alias %q is also a canonical predicate; the alias is unreachable", alias)
		}
		if alias != relationshipKey(alias) {
			t.Errorf("alias key %q is not in normalized form (%q)", alias, relationshipKey(alias))
		}
	}
	for sym := range symmetricRelationships {
		if !relationshipSet[sym] {
			t.Errorf("symmetric predicate %q is not in Relationships", sym)
		}
	}
}

func TestSymmetricRelationshipsAreSorted(t *testing.T) {
	got := SymmetricRelationships()
	if len(got) != len(symmetricRelationships) {
		t.Fatalf("got %d predicates, want %d", len(got), len(symmetricRelationships))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("not sorted at %d: %q >= %q", i, got[i-1], got[i])
		}
	}
}
