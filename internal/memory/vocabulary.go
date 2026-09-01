package memory

// EntityTypes is the closed vocabulary of entity types. It is closed on purpose:
// the extraction prompt used to end with "if none fit, invent a short lowercase
// singular noun", and validEntityType only checked that a type looked like a
// slug, so the corpus grew 43 types including "substance", "timezone", "cms" and
// "alias". A type that appears once classifies nothing.
//
// Migration 008 folds the historical types onto this list and adds a CHECK
// constraint, so adding a type here means writing a migration too.
var EntityTypes = []string{
	"person",
	"organization",
	"team",
	"role",
	"project",
	"product",
	"technology",
	"tool",
	"file",
	"event",
	"location",
	"metric",
	"concept",
}

// FallbackEntityType receives anything the vocabulary cannot place. It is a real
// type rather than a rejection: the relation is the valuable part of an
// extraction, and dropping it because the model coined an odd type loses more
// than mislabelling it does.
const FallbackEntityType = "concept"

// entityTypeAliases maps every type observed in the corpus onto the vocabulary,
// plus the obvious synonyms a model is likely to reach for next. Keys are
// already normalized.
var entityTypeAliases = map[string]string{
	// people and groups
	"department": "team",
	"group":      "team",
	"company":    "organization",
	"client":     "organization",
	"vendor":     "organization",
	"job":        "role",
	"position":   "role",

	// efforts
	"initiative": "project",
	"program":    "project",
	"engagement": "project",
	"campaign":   "project",

	// things built or sold
	"application": "product",
	"app":         "product",
	"service":     "product",
	"system":      "product",

	// the technical stack
	"platform":  "technology",
	"model":     "technology",
	"library":   "technology",
	"framework": "technology",
	"language":  "technology",
	"database":  "technology",
	"component": "technology",
	"class":     "technology",
	"function":  "technology",
	"endpoint":  "technology",
	"server":    "technology",
	"cms":       "technology",
	"protocol":  "technology",
	"format":    "technology",

	// things that do work for you
	"agent":       "tool",
	"software":    "tool",
	"bot":         "tool",
	"integration": "tool",

	// documents and artifacts
	"asset":     "file",
	"script":    "file",
	"directory": "file",
	"document":  "file",
	"folder":    "file",
	"report":    "file",
	"template":  "file",

	// occurrences
	"meeting":   "event",
	"milestone": "event",
	"date":      "event",

	// places
	"place":   "location",
	"region":  "location",
	"country": "location",
	"market":  "location",

	// measures
	"kpi":         "metric",
	"measurement": "metric",
	"goal":        "metric",
	"target":      "metric",

	// everything abstract — the long tail that motivated closing the list
	"activity":  "concept",
	"approach":  "concept",
	"resource":  "concept",
	"channel":   "concept",
	"practice":  "concept",
	"substance": "concept",
	"layout":    "concept",
	"content":   "concept",
	"property":  "concept",
	"case":      "concept",
	"alias":     "concept",
	"timezone":  "concept",
	"process":   "concept",
	"method":    "concept",
	"topic":     "concept",
	"idea":      "concept",
	"strategy":  "concept",
	"other":     "concept",
}

// PromptEntityTypes is the list offered to an extractor: the vocabulary minus
// the fallback. The fallback stays available to CanonicalEntityType for folding
// a type the model coins anyway, but naming it in the prompt made it the cheap
// answer — 35% of the corpus landed on "concept", carrying entries like "layered
// approach" and "bulk assign" that the prompt's own rules forbid. A model that
// is not offered a catch-all has to pick a real type or drop the entity, which
// is the intended outcome.
var PromptEntityTypes = func() []string {
	out := make([]string, 0, len(EntityTypes))
	for _, t := range EntityTypes {
		if t != FallbackEntityType {
			out = append(out, t)
		}
	}
	return out
}()

// entityTypeSet is EntityTypes as a lookup, built once.
var entityTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(EntityTypes))
	for _, t := range EntityTypes {
		m[t] = true
	}
	return m
}()

// CanonicalEntityType maps a type produced by an extractor onto the vocabulary.
// It returns the canonical type and whether the input was recognized — an
// unrecognized type still yields FallbackEntityType, so callers can record the
// relation and log the drift rather than choosing between the two.
func CanonicalEntityType(t string) (string, bool) {
	n := NormalizeName(t)
	if n == "" {
		return FallbackEntityType, false
	}
	if entityTypeSet[n] {
		return n, true
	}
	if alias, ok := entityTypeAliases[n]; ok {
		return alias, true
	}
	return FallbackEntityType, false
}
