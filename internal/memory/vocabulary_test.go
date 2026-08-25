package memory

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// historicalEntityTypes is every entity_type present in the live corpus before
// the vocabulary was closed — 43 of them. Migration 008 folds these, so each one
// must land somewhere sensible rather than being silently dropped.
var historicalEntityTypes = map[string]string{
	"concept": "concept", "project": "project", "tool": "tool",
	"technology": "technology", "person": "person", "organization": "organization",
	"file": "file", "product": "product", "team": "team", "role": "role",
	"metric": "metric", "event": "event", "location": "location",

	"activity": "concept", "approach": "concept", "resource": "concept",
	"channel": "concept", "practice": "concept", "substance": "concept",
	"layout": "concept", "content": "concept", "property": "concept",
	"case": "concept", "alias": "concept", "timezone": "concept",

	"asset": "file", "script": "file", "directory": "file",
	"initiative": "project", "department": "team", "application": "product",
	"agent": "tool",

	"model": "technology", "platform": "technology", "library": "technology",
	"language": "technology", "component": "technology", "class": "technology",
	"function": "technology", "endpoint": "technology", "database": "technology",
	"cms": "technology", "server": "technology",
}

func TestCanonicalEntityTypeCoversHistoricalCorpus(t *testing.T) {
	if len(historicalEntityTypes) != 43 {
		t.Fatalf("expected the 43 observed types, have %d", len(historicalEntityTypes))
	}
	for in, want := range historicalEntityTypes {
		got, ok := CanonicalEntityType(in)
		if !ok {
			t.Errorf("CanonicalEntityType(%q) reported unrecognized; every historical type should map", in)
		}
		if got != want {
			t.Errorf("CanonicalEntityType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalEntityTypeAlwaysReturnsVocabularyMember(t *testing.T) {
	inputs := []string{"", "   ", "Person", " TECHNOLOGY ", "wibble", "other", "a-type-nobody-has-seen"}
	for _, in := range inputs {
		got, _ := CanonicalEntityType(in)
		if !entityTypeSet[got] {
			t.Errorf("CanonicalEntityType(%q) = %q, which is not in EntityTypes", in, got)
		}
	}
}

func TestCanonicalEntityTypeSignalsDrift(t *testing.T) {
	if _, ok := CanonicalEntityType("quantum-widget"); ok {
		t.Error("an unknown type should report ok=false so the drift gets logged")
	}
	if got, _ := CanonicalEntityType("quantum-widget"); got != FallbackEntityType {
		t.Errorf("unknown type = %q, want %q", got, FallbackEntityType)
	}
	if _, ok := CanonicalEntityType("Department"); !ok {
		t.Error("a known alias should report ok=true regardless of casing")
	}
}

// Every alias must point at a real vocabulary member, or the CHECK constraint in
// migration 008 rejects the insert at runtime.
func TestEntityTypeAliasesResolve(t *testing.T) {
	for alias, target := range entityTypeAliases {
		if !entityTypeSet[target] {
			t.Errorf("alias %q maps to %q, which is not in EntityTypes", alias, target)
		}
		if entityTypeSet[alias] {
			t.Errorf("alias %q is already a vocabulary member; the alias is dead", alias)
		}
	}
}

// The CHECK constraint in migration 008 hard-codes the vocabulary. If EntityTypes
// gains a value without a matching migration, writes fail against a migrated
// database — so the two lists are compared here rather than in production.
func TestMigrationCheckMatchesVocabulary(t *testing.T) {
	path := filepath.Join("..", "..", "migrations", "008_entity_type_vocabulary.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	re := regexp.MustCompile(`(?s)ADD CONSTRAINT entities_entity_type_check CHECK \(\s*entity_type IN \((.*?)\)`)
	m := re.FindSubmatch(body)
	if m == nil {
		t.Fatal("could not find the entity_type CHECK constraint in migration 008")
	}

	var inSQL []string
	for _, part := range strings.Split(string(m[1]), ",") {
		if v := strings.Trim(strings.TrimSpace(part), "'"); v != "" {
			inSQL = append(inSQL, v)
		}
	}

	want := append([]string(nil), EntityTypes...)
	sort.Strings(want)
	sort.Strings(inSQL)

	if strings.Join(want, ",") != strings.Join(inSQL, ",") {
		t.Errorf("migration 008 CHECK list and memory.EntityTypes disagree:\n  SQL: %v\n  Go:  %v", inSQL, want)
	}
}
