package memory

import (
	"strings"
	"testing"
)

// tsqueryExpr output is interpolated into SQL, so it must only ever come from
// this closed set of literals — never from anything caller-supplied.
func TestTsqueryExprIsAClosedSet(t *testing.T) {
	known := map[FullTextMode]string{
		FullTextAll:       "plainto_tsquery",
		FullTextAny:       "to_tsquery",
		FullTextWebsearch: "websearch_to_tsquery",
		FullTextOff:       "NULLIF",
	}

	for mode, want := range known {
		expr := mode.TsqueryExpr()
		if !strings.Contains(expr, want) {
			t.Errorf("%s: expression %q does not use %s", mode, expr, want)
		}
		if !strings.Contains(expr, "$4") {
			t.Errorf("%s: expression %q must reference the bound parameter, "+
				"or Postgres cannot infer its type", mode, expr)
		}
	}

	// An unrecognised mode must fall back to a safe literal rather than
	// leaking its own value into the SQL.
	hostile := FullTextMode("'; DROP TABLE memories; --")
	expr := hostile.TsqueryExpr()
	if strings.Contains(expr, "DROP") {
		t.Fatalf("unknown mode leaked into SQL: %q", expr)
	}
	if expr != FullTextMode("").TsqueryExpr() {
		t.Errorf("unknown mode did not use the default expression: %q", expr)
	}
}

// The boosts are documented as tiebreakers, which only holds if they stay
// small next to the RRF terms that k scales.
func TestMetadataBoostsStaySubordinateToRRF(t *testing.T) {
	cfg := DefaultScoringConfig

	// Largest possible combined metadata contribution.
	maxBoost := cfg.ImportanceWeight + cfg.RecencyWeight + cfg.AccessWeight

	// RRF score for a result ranked first by both halves.
	topRRF := 1/float64(cfg.RRFConstant+1) + cfg.FullTextWeight/float64(cfg.RRFConstant+1)

	if maxBoost > topRRF/5 {
		t.Errorf("metadata boosts (%.4f) are not a tiebreaker against a top RRF score of %.4f; "+
			"either lower the weights or lower RRFConstant (currently %d)",
			maxBoost, topRRF, cfg.RRFConstant)
	}
}
