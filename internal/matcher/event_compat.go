// Package matcher — event_compat.go contains signature compatibility checks
// and entity normalization for event-level matching.
package matcher

// --- Entity normalization ---

// entitySynonyms maps entity fragments to their canonical form.
// This resolves cases where "fed" and "reserve" should both become "federal reserve".
var entitySynonyms = map[string]string{
	"fed":    "federal reserve",
	"fomc":   "federal reserve",
	"reserve": "federal reserve",
	"btc":    "bitcoin",
	"eth":    "ethereum",
	"sol":    "solana",
	"doge":   "dogecoin",
	"ada":    "cardano",
	"xrp":    "ripple",
	"gop":    "republican",
	"dem":    "democrat",
	"dems":   "democrat",
	"potus":  "president",
	"scotus": "supreme court",
}

// normalizeEntities resolves entity synonyms and deduplicates.
func normalizeEntities(entities []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, e := range entities {
		// Check synonym mapping
		if canonical, ok := entitySynonyms[e]; ok {
			e = canonical
		}
		if !seen[e] {
			seen[e] = true
			result = append(result, e)
		}
	}
	return result
}

// --- Signature compatibility checks ---

// SignaturesCompatible returns true if two event signatures could plausibly
// represent the same market. Used as a hard gate before scoring.
//
// Rules:
//   - If both have entities, at least one must overlap
//   - If both have thresholds, they must be equal
//   - If both have comparators, they must be compatible (same or equivalent)
//   - If both have market types, they must match
//   - If both have actions and they're fundamentally different (e.g. "win" vs "impeach"), reject
func SignaturesCompatible(a, b *EventSignature) bool {
	// Entity overlap check: if both have entities, at least one must match
	if len(a.Entities) > 0 && len(b.Entities) > 0 {
		setA := make(map[string]bool, len(a.Entities))
		for _, e := range a.Entities {
			setA[e] = true
		}
		overlap := false
		for _, e := range b.Entities {
			if setA[e] {
				overlap = true
				break
			}
		}
		if !overlap {
			return false
		}
	}

	// Threshold must match when both present
	if a.Threshold != "" && b.Threshold != "" && a.Threshold != b.Threshold {
		return false
	}

	// Market type must match when both present and non-default
	if a.MarketType != "" && b.MarketType != "" &&
		a.MarketType != "binary" && b.MarketType != "binary" &&
		a.MarketType != b.MarketType {
		return false
	}

	// Comparator compatibility: opposite directions are incompatible
	if a.Comparator != "" && b.Comparator != "" {
		if !comparatorsCompatible(a.Comparator, b.Comparator) {
			return false
		}
	}

	// Action compatibility: fundamentally different actions on the same entity
	// are different questions (e.g. "win" vs "impeach", "cut" vs "raise")
	if a.Action != "" && b.Action != "" && a.Action != b.Action {
		if !actionsCompatible(a.Action, b.Action) {
			return false
		}
	}

	return true
}

// actionsCompatible returns true if two actions could describe the same event.
// E.g., "hit" and "reach" are compatible; "win" and "impeach" are not.
func actionsCompatible(a, b string) bool {
	if a == b {
		return true
	}
	groups := [][]string{
		{"hit", "reach", "exceed", "rise"},
		{"drop", "fall"},
		{"cut", "lower"},
		{"raise", "hike"},
		{"win", "elect"},
		{"ban", "block"},
		{"pass", "approve", "sign"},
	}
	for _, g := range groups {
		inA, inB := false, false
		for _, v := range g {
			if v == a {
				inA = true
			}
			if v == b {
				inB = true
			}
		}
		if inA && inB {
			return true
		}
	}
	return false
}

// actionsIncompatible returns true when two extracted actions belong to
// different intent groups (e.g., "win" is outcome but "host" is location).
// Returns false if either action is empty or if both are in the same group.
func actionsIncompatible(actA, actB string) bool {
	if actA == "" || actB == "" {
		return false
	}
	if actA == actB {
		return false
	}
	groupA := actionIntentGroups[actA]
	groupB := actionIntentGroups[actB]
	if groupA == "" || groupB == "" {
		// Unknown group — fall back to simple inequality
		return actA != actB
	}
	return groupA != groupB
}

// comparatorsCompatible checks if two comparators could describe the same question.
func comparatorsCompatible(a, b string) bool {
	if a == b {
		return true
	}
	// Group equivalent comparators
	upward := map[string]bool{"reach": true, "exceed": true, "rise_above": true, "stay_above": true, "close_above": true}
	downward := map[string]bool{"drop_below": true, "stay_below": true, "close_below": true}

	// Same direction is compatible (e.g., "reach" and "exceed" ask similar questions)
	if upward[a] && upward[b] {
		return true
	}
	if downward[a] && downward[b] {
		return true
	}
	// Cross-direction is incompatible
	if (upward[a] && downward[b]) || (downward[a] && upward[b]) {
		return false
	}
	// Unknown comparators are permissive
	return true
}
