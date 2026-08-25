package memory

import (
	"reflect"
	"testing"
)

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Acme", "acme"},
		{"ACME", "acme"},
		{"  Platform   Team  ", "platform team"},
		{"Platform team", "platform team"},
		{"draft slides", "draft slides"},
		{"", ""},
		{"   ", ""},
		{"\tGemini\nVision ", "gemini vision"},
	}
	for _, tt := range tests {
		if got := NormalizeName(tt.in); got != tt.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The variants below are real pairs from the live corpus, where they had become
// separate entities and separate tags.
func TestNormalizeNameCollapsesRealVariants(t *testing.T) {
	groups := [][]string{
		{"Acme", "ACME"},
		{"platform team", "Platform team", "Platform Team"},
		{"ada", "Ada"},
		{"self-service", "Self-service"},
		{"Quarterly report", "quarterly report"},
	}
	for _, g := range groups {
		first := NormalizeName(g[0])
		for _, v := range g[1:] {
			if got := NormalizeName(v); got != first {
				t.Errorf("NormalizeName(%q) = %q, want %q (same thing as %q)", v, got, first, g[0])
			}
		}
	}
}

func TestNormalizeTags(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil stays nil", nil, nil},
		{"empty stays nil", []string{}, nil},
		{"case folded", []string{"acme", "ACME"}, []string{"acme"}},
		{"order preserved", []string{"project", "architecture"}, []string{"project", "architecture"}},
		{"blanks dropped", []string{"", "  ", "go"}, []string{"go"}},
		{"all blank becomes nil", []string{"", "   "}, nil},
		{"inner whitespace collapsed", []string{"meeting  notes"}, []string{"meeting notes"}},
		{"duplicates after folding", []string{"Ada", "ada", "ADA"}, []string{"ada"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeTags(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NormalizeTags(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// NormalizeTags must not write through the caller's slice: StoreMemory hands it
// whatever the MCP client sent.
func TestNormalizeTagsDoesNotMutateInput(t *testing.T) {
	in := []string{"Acme", "Project"}
	_ = NormalizeTags(in)
	if in[0] != "Acme" || in[1] != "Project" {
		t.Errorf("input mutated: %q", in)
	}
}
