package memory

import (
	"sort"
	"strings"
)

// Relationships is the closed vocabulary of relationship predicates. It is
// closed for the same reason EntityTypes is, only the symptom was worse: the
// prompt asked for "a short verb phrase" and the corpus answered with 503
// distinct labels over 1272 edges, 341 of them used exactly once. "holds",
// "holds role" and "holds-role" sat on the same pair of nodes as three separate
// edges. A predicate that appears once relates nothing.
//
// Each predicate has one canonical direction, written subject -> object below.
// The inverse spellings a model reaches for ("employs", "used-by", "led-by")
// fold onto the canonical predicate with the endpoints swapped, which is what
// collapses the 111 pairs that were stored in both directions.
//
// Migration 010 folds the historical labels onto this list and adds a CHECK
// constraint, so adding a predicate here means writing a migration too.
var Relationships = []string{
	// identity and structure
	"is-a",       // subject is an instance/kind of object
	"same-as",    // subject and object are one thing under two names
	"part-of",    // subject is contained by object
	"located-in", // subject sits at object
	// people
	"works-at",
	"leads",
	"has-role",
	"collaborates-with",
	"mentors",
	"family-of",
	// effort
	"works-on",
	"creates",
	"owns",
	"provides",
	"targets",
	// technical
	"uses",
	"depends-on",
	"integrates-with",
	"implements",
	"hosted-on",
	"stores",
	// judgement
	"supersedes",
	"compares-with",
	"contradicts",
	"prefers",
	"avoids",
	"measured-by",
	// the catch-all
	"relates-to",
}

// FallbackRelationship receives anything the vocabulary cannot place. As with
// FallbackEntityType it is a real predicate rather than a rejection: the edge
// itself is the valuable part, and the model's own wording is kept verbatim in
// relations.relationship_raw, so nothing is actually lost by folding.
const FallbackRelationship = "relates-to"

// symmetricRelationships hold in both directions. Endpoint order carries no
// meaning for them, so writers put the endpoints in a fixed order to stop
// "A same-as B" and "B same-as A" becoming two edges.
var symmetricRelationships = map[string]bool{
	"same-as":           true,
	"collaborates-with": true,
	"family-of":         true,
	"integrates-with":   true,
	"compares-with":     true,
	"contradicts":       true,
	// The fallback says only that two things are connected, which is as true
	// one way as the other. Left asymmetric it re-created the duplicate it is
	// supposed to absorb.
	"relates-to": true,
}

// canonicalRelation is a vocabulary predicate plus whether reaching it means
// swapping the relation's endpoints.
type canonicalRelation struct {
	label    string
	reversed bool
}

// relationshipAliases maps every label observed in the corpus onto the
// vocabulary, plus the obvious synonyms a model is likely to reach for next.
// Keys are in normalized form (see relationshipKey). reversed: true means the
// alias states the relation backwards from the canonical direction.
var relationshipAliases = map[string]canonicalRelation{
	// --- is-a -------------------------------------------------------------
	"is":                {"is-a", false},
	"type-of":           {"is-a", false},
	"is-type-of":        {"is-a", false},
	"is-an-instance-of": {"is-a", false},
	"classified-as":     {"is-a", false},
	"kind-of":           {"is-a", false},
	"exemplifies":       {"is-a", false},
	"exemplified-by":    {"is-a", true},
	"represents":        {"is-a", false},

	// --- same-as ----------------------------------------------------------
	"identical":           {"same-as", false},
	"is-same-as":          {"same-as", false},
	"known-as":            {"same-as", false},
	"is-known-as":         {"same-as", false},
	"commonly-called":     {"same-as", false},
	"is-officially-named": {"same-as", false},
	"stands-for":          {"same-as", false},
	"abbreviates":         {"same-as", false},
	"alias-of":            {"same-as", false},
	"represented-by":      {"same-as", true},

	// --- part-of ----------------------------------------------------------
	"belongs-to":          {"part-of", false},
	"member-of":           {"part-of", false},
	"is-member-of":        {"part-of", false},
	"is-part-of":          {"part-of", false},
	"is-pillar-of":        {"part-of", false},
	"is-use-case-of":      {"part-of", false},
	"includes":            {"part-of", true},
	"includes-component":  {"part-of", true},
	"includes-response":   {"part-of", true},
	"includes-pillar":     {"part-of", true},
	"contains":            {"part-of", true},
	"contains-department": {"part-of", true},
	"comprises":           {"part-of", true},
	"encompasses":         {"part-of", true},
	"has":                 {"part-of", true},
	"has-department":      {"part-of", true},
	"has-use-case":        {"part-of", true},
	"has-project":         {"part-of", true},
	"has-part":            {"part-of", true},

	// --- located-in -------------------------------------------------------
	"at":                   {"located-in", false},
	"based-in":             {"located-in", false},
	"is-based-in":          {"located-in", false},
	"located-at":           {"located-in", false},
	"located-in-region-of": {"located-in", false},
	"available-in":         {"located-in", false},
	"listed-on":            {"located-in", false},

	// --- works-at ---------------------------------------------------------
	"works-for":   {"works-at", false},
	"worked-for":  {"works-at", false},
	"works-in":    {"works-at", false},
	"employed-by": {"works-at", false},
	"client-of":   {"works-at", false},
	"employs":     {"works-at", true},
	"staffed-by":  {"works-at", true},

	// --- leads ------------------------------------------------------------
	"manages":        {"leads", false},
	"supervises":     {"leads", false},
	"steers":         {"leads", false},
	"led":            {"leads", false},
	"leads-in":       {"leads", false},
	"led-by":         {"leads", true},
	"lead-by":        {"leads", true},
	"is-led-by":      {"leads", true},
	"managed-by":     {"leads", true},
	"reports-to":     {"leads", true},
	"accountable-to": {"leads", true},

	// --- has-role ---------------------------------------------------------
	"holds":           {"has-role", false},
	"holds-role":      {"has-role", false},
	"holds-role-as":   {"has-role", false},
	"currently-holds": {"has-role", false},
	"acts-as":         {"has-role", false},
	"plays":           {"has-role", false},
	"works-in-role":   {"has-role", false},
	"held-by":         {"has-role", true},

	// --- collaborates-with ------------------------------------------------
	"works-with":       {"collaborates-with", false},
	"co-worked-with":   {"collaborates-with", false},
	"collaborates-on":  {"collaborates-with", false},
	"coordinates-with": {"collaborates-with", false},
	"contacts":         {"collaborates-with", false},
	"has-partner":      {"collaborates-with", false},

	// --- mentors ----------------------------------------------------------
	"mentor":    {"mentors", false},
	"mentored":  {"mentors", false},
	"taught-by": {"mentors", true},

	// --- family-of --------------------------------------------------------
	"aunt":         {"family-of", false},
	"aunt-of":      {"family-of", false},
	"is-aunt-of":   {"family-of", false},
	"cousin-of":    {"family-of", false},
	"is-cousin-of": {"family-of", false},
	"has-cousin":   {"family-of", false},

	// --- works-on ---------------------------------------------------------
	"worked-on":      {"works-on", false},
	"working-on":     {"works-on", false},
	"handles":        {"works-on", false},
	"analyzes":       {"works-on", false},
	"evaluates":      {"works-on", false},
	"specializes-in": {"works-on", false},
	"contributes-to": {"works-on", false},
	"processes":      {"works-on", false},
	"performs":       {"works-on", false},
	"executes":       {"works-on", false},
	"explores":       {"works-on", false},
	"discusses":      {"works-on", false},

	// --- creates ----------------------------------------------------------
	"created":         {"creates", false},
	"builds":          {"creates", false},
	"built":           {"creates", false},
	"built-as":        {"creates", false},
	"develops":        {"creates", false},
	"designs":         {"creates", false},
	"designed-as":     {"creates", false},
	"generates":       {"creates", false},
	"produces":        {"creates", false},
	"defines":         {"creates", false},
	"established":     {"creates", false},
	"created-by":      {"creates", true},
	"built-by":        {"creates", true},
	"rebuilt-by":      {"creates", true},
	"defined-by":      {"creates", true},
	"established-by":  {"creates", true},
	"instantiated-by": {"creates", true},

	// --- owns -------------------------------------------------------------
	"responsible-for": {"owns", false},
	"owner-of":        {"owns", false},
	"maintains":       {"owns", false},
	"owned-by":        {"owns", true},

	// --- provides ---------------------------------------------------------
	"supports":         {"provides", false},
	"serves":           {"provides", false},
	"delivers":         {"provides", false},
	"offers-service":   {"provides", false},
	"provides-service": {"provides", false},
	"distributes-to":   {"provides", false},
	"distributes-via":  {"provides", false},
	"ships-to":         {"provides", false},
	"sells":            {"provides", false},
	"enables":          {"provides", false},
	"powers":           {"provides", false},
	"supported-by":     {"provides", true},
	"enabled-by":       {"provides", true},
	"provided-by":      {"provides", true},

	// --- targets ----------------------------------------------------------
	"focuses-on":      {"targets", false},
	"aims-to":         {"targets", false},
	"aims-to-build":   {"targets", false},
	"has-goal-of":     {"targets", false},
	"addresses":       {"targets", false},
	"intends-to-buy":  {"targets", false},
	"designed-for":    {"targets", false},
	"is-designed-for": {"targets", false},
	"recommended-for": {"targets", false},

	// --- uses -------------------------------------------------------------
	"utilizes":      {"uses", false},
	"using":         {"uses", false},
	"leverages":     {"uses", false},
	"built-with":    {"uses", false},
	"based-on":      {"uses", false},
	"is-built-on":   {"uses", false},
	"builds-on":     {"uses", false},
	"written-in":    {"uses", false},
	"operates-on":   {"uses", false},
	"runs":          {"uses", false},
	"accessed-via":  {"uses", false},
	"accesses":      {"uses", false},
	"uses-provider": {"uses", false},
	"uses-for":      {"uses", false},
	"applies-to":    {"uses", false},
	"used-by":       {"uses", true},
	"used-in":       {"uses", true},
	"used-for":      {"uses", true},
	"used-on":       {"uses", true},
	"accessed-by":   {"uses", true},

	// --- depends-on -------------------------------------------------------
	"requires":   {"depends-on", false},
	"relies-on":  {"depends-on", false},
	"needs":      {"depends-on", false},
	"blocked-by": {"depends-on", false},

	// --- integrates-with --------------------------------------------------
	"integrates":       {"integrates-with", false},
	"connects-to":      {"integrates-with", false},
	"connected-to":     {"integrates-with", false},
	"interacts-with":   {"integrates-with", false},
	"interacts-via":    {"integrates-with", false},
	"syncs-with":       {"integrates-with", false},
	"synchronizes":     {"integrates-with", false},
	"sends-events-to":  {"integrates-with", false},
	"sends-metrics-to": {"integrates-with", false},
	"sends-traces-to":  {"integrates-with", false},
	"compatible-with":  {"integrates-with", false},

	// --- implements -------------------------------------------------------
	"implemented-as":    {"implements", false},
	"is-implemented-as": {"implements", false},
	"operationalizes":   {"implements", false},
	"is-implemented-by": {"implements", true},
	"implementing":      {"implements", false},
	"is-implementing":   {"implements", false},

	// --- hosted-on --------------------------------------------------------
	"runs-on": {"hosted-on", false},
	"hosts":   {"hosted-on", true},

	// --- stores -----------------------------------------------------------
	"saves":     {"stores", false},
	"stores-in": {"stores", true},

	// --- supersedes -------------------------------------------------------
	"replaces":            {"supersedes", false},
	"is-phasing-out":      {"supersedes", false},
	"moves-away-from":     {"supersedes", false},
	"replaced-by":         {"supersedes", true},
	"superseded-by":       {"supersedes", true},
	"migrates-to":         {"supersedes", true},
	"migrating-to":        {"supersedes", true},
	"transitioning-to":    {"supersedes", true},
	"is-transitioning-to": {"supersedes", true},
	"shifts-to":           {"supersedes", true},

	// --- compares-with ----------------------------------------------------
	"compares":            {"compares-with", false},
	"compared-to":         {"compares-with", false},
	"compared-with":       {"compares-with", false},
	"compared-against":    {"compares-with", false},
	"similar-to":          {"compares-with", false},
	"competes-with":       {"compares-with", false},
	"competing-platforms": {"compares-with", false},
	"competitor-of":       {"compares-with", false},
	"alternative-to":      {"compares-with", false},
	"overlaps-with":       {"compares-with", false},

	// --- prefers / avoids -------------------------------------------------
	"likes":             {"prefers", false},
	"loves":             {"prefers", false},
	"prefers-over":      {"prefers", false},
	"preferred-for":     {"prefers", false},
	"preferred-over":    {"prefers", false},
	"prefers-receiving": {"prefers", false},
	"preferred-by":      {"prefers", true},
	"does-not-prefer":   {"avoids", false},
	"dislikes":          {"avoids", false},
	"finds-unfeasible":  {"avoids", false},
	"lacks":             {"avoids", false},

	// --- measured-by ------------------------------------------------------
	"measures":         {"measured-by", true},
	"monitors":         {"measured-by", true},
	"monitored-by":     {"measured-by", false},
	"tracks":           {"measured-by", true},
	"tracked-on":       {"measured-by", false},
	"has-target-value": {"measured-by", false},

	// --- billing, access and the rest of the observed tail ----------------
	"bills-to":              {"provides", false},
	"billed-for":            {"provides", false},
	"billed-under":          {"part-of", false},
	"has-permission":        {"uses", false},
	"allows-access":         {"uses", true},
	"egress-via":            {"uses", false},
	"operates":              {"uses", false},
	"uploads":               {"stores", false},
	"uploads-to":            {"stores", true},
	"submits-to":            {"stores", true},
	"submitted-to":          {"stores", true},
	"protects":              {"provides", false},
	"shortlists":            {"prefers", false},
	"excludes":              {"avoids", false},
	"interested-in":         {"targets", false},
	"should-use-instead-of": {"supersedes", false},
	"defined":               {"creates", false},
	"completed":             {"works-on", false},
	"improving":             {"works-on", false},
	"improves":              {"works-on", false},
	"standardizing":         {"works-on", false},

	// --- explicit catch-alls ---------------------------------------------
	// These arrive often enough to be worth naming, but say nothing more
	// specific than the fallback does.
	"related-to":                 {"relates-to", false},
	"refers-to":                  {"relates-to", false},
	"involves":                   {"relates-to", false},
	"mentioned":                  {"relates-to", false},
	"discussed":                  {"relates-to", false},
	"covers":                     {"relates-to", false},
	"associated-with":            {"relates-to", false},
	"referenced-in":              {"relates-to", false},
	"maps":                       {"relates-to", false},
	"identifies":                 {"relates-to", false},
	"indicates":                  {"relates-to", false},
	"considers":                  {"relates-to", false},
	"reports":                    {"relates-to", false},
	"followed-by":                {"relates-to", false},
	"contact-for":                {"relates-to", false},
	"stakeholder-of":             {"relates-to", false},
	"activates-across":           {"relates-to", false},
	"being-evaluated-for":        {"relates-to", false},
	"struggles-with":             {"relates-to", false},
	"unauthorized-access-to":     {"relates-to", false},
	"provides-information-about": {"relates-to", false},
	"discussed-by":               {"relates-to", false},
}

// relationshipSet is Relationships as a lookup, built once.
var relationshipSet = func() map[string]bool {
	m := make(map[string]bool, len(Relationships))
	for _, r := range Relationships {
		m[r] = true
	}
	return m
}()

// relationshipKey folds a label to the form the vocabulary is keyed by.
// NormalizeName already lowercases and turns hyphens and underscores into
// spaces, so "holds role", "holds-role" and "HOLDS_ROLE" arrive here as one
// string; joining on hyphens restores the spelling the vocabulary is written in.
func relationshipKey(s string) string {
	n := NormalizeName(s)
	if n == "" {
		return ""
	}
	return strings.ReplaceAll(n, " ", "-")
}

// CanonicalRelationship maps a label produced by an extractor onto the
// vocabulary. It returns the canonical predicate, whether the endpoints must be
// swapped to match that predicate's direction, and whether the label was
// recognized. An unrecognized label still yields FallbackRelationship, so the
// caller can keep the edge and log the drift rather than choosing between them.
func CanonicalRelationship(s string) (label string, reversed bool, ok bool) {
	k := relationshipKey(s)
	if k == "" {
		return FallbackRelationship, false, false
	}
	if relationshipSet[k] {
		return k, false, true
	}
	if c, found := relationshipAliases[k]; found {
		return c.label, c.reversed, true
	}
	return FallbackRelationship, false, false
}

// SymmetricRelationships lists the predicates that hold in both directions,
// sorted so the result is stable. Storage passes it to SQL that has to apply the
// same endpoint ordering, which keeps this file the single source of truth.
func SymmetricRelationships() []string {
	out := make([]string, 0, len(symmetricRelationships))
	for r := range symmetricRelationships {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// IsSymmetricRelationship reports whether a canonical predicate holds equally in
// both directions, in which case writers normalize the endpoint order.
func IsSymmetricRelationship(label string) bool {
	return symmetricRelationships[label]
}
