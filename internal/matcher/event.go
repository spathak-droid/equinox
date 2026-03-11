// Package matcher — event.go implements structured event extraction from market titles.
//
// This separates "same event" from "similar topic" by extracting:
//   - Entity: who/what the market is about (e.g., "bitcoin", "trump")
//   - Action: what is being predicted (e.g., "reach", "win", "cut")
//   - Threshold: numeric target (e.g., "100000", "5")
//   - Direction: above/below/at semantic (e.g., "above", "under")
//   - DateRef: any date reference in the title (e.g., "2026", "june")
//   - Metric: what dimension is being measured (e.g., "price", "rate", "votes")
//   - Comparator: the comparison semantics (e.g., "reach", "exceed", "stay_above")
//   - MarketType: structural type (e.g., "binary", "range")
//   - Region: geographic scope when detectable (e.g., "us", "global")
//
// Two markets are the "same event" when their canonical signatures match.
// Two markets are "similar topic" when they share entities but differ on
// thresholds, dates, or direction.
package matcher

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// EventSignature is a structured representation of what a market is asking.
// Extracted purely from the normalized title — no AI needed.
type EventSignature struct {
	Entities  []string // key subjects (e.g., ["bitcoin"], ["trump"])
	Action    string   // predicted action (e.g., "reach", "win", "cut")
	Threshold string   // numeric target, normalized (e.g., "100000", "5")
	Direction string   // "above", "below", "at", "" (unknown)
	DateRef   string   // date reference from title (e.g., "2026", "june 2026")

	// Semantic fields added for deterministic matching
	Metric     string  // what's being measured: "price", "rate", "votes", "market_cap", "count", ""
	Comparator string  // normalized comparison: "reach", "exceed", "drop_below", "stay_above", "win", ""
	MarketType string  // "binary", "range", "multi" — defaults to "binary"
	Region     string  // geographic scope: "us", "eu", "global", "" (unknown)
	Confidence float64 // parse confidence [0.0, 1.0] — how many fields were extracted
}

// Key returns a deterministic string key for comparing event signatures.
// Two markets with the same Key() are asking the same question.
func (e *EventSignature) Key() string {
	entStr := strings.Join(e.Entities, "+")
	return fmt.Sprintf("%s|%s|%s|%s|%s", entStr, e.Action, e.Threshold, e.Direction, e.DateRef)
}

// CanonicalSignature returns a SHA-256 hash of the normalized semantic fields.
// Two markets with the same CanonicalSignature are asking the same question
// and can be matched instantly without scoring.
//
// Returns "" if insufficient fields are populated for a reliable signature
// (we require at least entity + one of: threshold, action, or metric).
func (e *EventSignature) CanonicalSignature() string {
	if len(e.Entities) == 0 {
		return "" // no entity = no reliable signature
	}
	// Require at least one discriminating field beyond entity
	if e.Threshold == "" && e.Action == "" && e.Metric == "" {
		return ""
	}

	// Sort entities for determinism
	sorted := make([]string, len(e.Entities))
	copy(sorted, e.Entities)
	sort.Strings(sorted)

	// Build canonical form: entity|metric|comparator|threshold|direction|dateref
	parts := []string{
		strings.Join(sorted, "+"),
		e.Metric,
		e.Comparator,
		e.Threshold,
		e.Direction,
		e.DateRef,
	}
	raw := strings.Join(parts, "|")

	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16]) // 128-bit is sufficient for dedup
}

// FilledFieldCount returns how many semantic fields were successfully extracted.
func (e *EventSignature) FilledFieldCount() int {
	count := 0
	if len(e.Entities) > 0 {
		count++
	}
	if e.Action != "" {
		count++
	}
	if e.Threshold != "" {
		count++
	}
	if e.Direction != "" {
		count++
	}
	if e.DateRef != "" {
		count++
	}
	if e.Metric != "" {
		count++
	}
	if e.Comparator != "" {
		count++
	}
	if e.Region != "" {
		count++
	}
	return count
}

// ExtractEventSignature parses a market title into structured fields.
// Works on the raw title (not pre-normalized) to preserve original phrasing.
func ExtractEventSignature(title string) *EventSignature {
	norm := strings.ToLower(strings.TrimSpace(title))

	sig := &EventSignature{
		MarketType: "binary", // default
	}

	// Extract entities (proper nouns from original title + topic keywords from normalized)
	rawEntities := extractEntities(title)
	normEntities := extractNormKeyEntities(title)
	entSet := map[string]bool{}
	for _, e := range rawEntities {
		e = strings.ToLower(e)
		if !entSet[e] {
			entSet[e] = true
			sig.Entities = append(sig.Entities, e)
		}
	}
	for _, e := range normEntities {
		e = strings.ToLower(e)
		if !entSet[e] {
			entSet[e] = true
			sig.Entities = append(sig.Entities, e)
		}
	}

	// Extract action verb
	sig.Action = extractAction(norm)

	// Extract direction
	sig.Direction = extractDirection(norm)

	// Extract threshold numbers from normalized title
	sig.Threshold = extractThreshold(norm)

	// Extract date references
	sig.DateRef = extractDateRef(norm)

	// Extract metric — what dimension is being measured
	sig.Metric = extractMetric(norm)

	// Extract comparator — normalized comparison semantics
	sig.Comparator = extractComparator(norm, sig.Action, sig.Direction)

	// Extract region — geographic scope
	sig.Region = extractRegion(norm)

	// Detect market type from title patterns
	sig.MarketType = detectMarketType(norm)

	// Normalize entities through synonym resolution so "fed" and "reserve"
	// both resolve to "federal reserve", and "btc" resolves to "bitcoin", etc.
	sig.Entities = normalizeEntities(sig.Entities)

	// Compute parse confidence based on how many fields we extracted
	total := 8.0 // entity, action, threshold, direction, dateref, metric, comparator, region
	sig.Confidence = float64(sig.FilledFieldCount()) / total

	return sig
}

// EventMatchScore compares two event signatures and returns a score in [0.0, 1.0].
// 1.0 = same event, 0.0 = completely different.
//
// This is used as an additional signal in the composite score to separate
// "same event" from "similar topic".
func EventMatchScore(a, b *EventSignature) float64 {
	score := 0.0
	weights := 0.0

	// Entity overlap (most important)
	if len(a.Entities) > 0 || len(b.Entities) > 0 {
		setA := map[string]bool{}
		for _, e := range a.Entities {
			setA[e] = true
		}
		inter := 0
		for _, e := range b.Entities {
			if setA[e] {
				inter++
			}
		}
		union := len(setA)
		for _, e := range b.Entities {
			if !setA[e] {
				union++
			}
		}
		if union > 0 {
			score += 0.30 * float64(inter) / float64(union)
		}
		weights += 0.30
	}

	// Threshold match (critical — $100k vs $150k is a different event)
	if a.Threshold != "" && b.Threshold != "" {
		if a.Threshold == b.Threshold {
			score += 0.30
		} else {
			// Different thresholds = definitely different events, even if everything else matches.
			// Heavy penalty: subtract from score AND add weight so it drags the normalized score down.
			score -= 0.30
		}
		weights += 0.30
	} else if a.Threshold == "" && b.Threshold == "" {
		// Both have no threshold — neutral
		score += 0.15
		weights += 0.30
	}

	// Direction match
	if a.Direction != "" && b.Direction != "" {
		if a.Direction == b.Direction {
			score += 0.10
		} else {
			score -= 0.05 // opposite direction = different question
		}
		weights += 0.10
	}

	// Action match — different actions on the same entity = different question
	if a.Action != "" && b.Action != "" {
		if a.Action == b.Action {
			score += 0.15
		} else {
			// Different action = likely different question (e.g. "win" vs "impeach")
			score -= 0.10
		}
		weights += 0.15
	}

	// Date reference match
	if a.DateRef != "" && b.DateRef != "" {
		if a.DateRef == b.DateRef {
			score += 0.15
		} else {
			score -= 0.05 // different date ref = different event
		}
		weights += 0.15
	}

	if weights == 0 {
		return 0.5 // no signal at all
	}

	// Normalize to [0, 1]
	result := score / weights
	if result < 0 {
		result = 0
	}
	if result > 1 {
		result = 1
	}
	return result
}

// --- Extraction helpers ---

// actionVerbs are verbs that indicate what is being predicted.
var actionVerbs = []string{
	"win", "reach", "hit", "exceed", "drop", "fall", "rise",
	"cut", "raise", "elect", "impeach", "resign", "announce",
	"approve", "ban", "pass", "sign", "launch", "default",
	"invade", "attack", "print", "close", "open", "trade",
}

func extractAction(norm string) string {
	words := strings.Fields(norm)
	for _, w := range words {
		// Apply synonym expansion to catch "reaches" → "reach" etc.
		if canonical, ok := synonyms[w]; ok {
			w = canonical
		}
		for _, verb := range actionVerbs {
			if w == verb {
				return verb
			}
		}
	}
	return ""
}

// directionWords map to a canonical direction.
var directionWords = map[string]string{
	"above": "above", "over": "above", "higher": "above",
	"greater": "above", "exceed": "above", "exceeds": "above",
	"reach": "above", "hit": "above", "hits": "above",
	"below": "below", "under": "below", "lower": "below",
	"less": "below", "drop": "below", "fall": "below",
}

func extractDirection(norm string) string {
	for _, w := range strings.Fields(norm) {
		if dir, ok := directionWords[w]; ok {
			return dir
		}
	}
	return ""
}

// numberPattern matches numbers like "100000", "100k", "1.5m", "$100,000", "5%"
// The suffix (k/m/b/%) must be immediately adjacent to the number with no space.
var numberPattern = regexp.MustCompile(`\$?([\d,]+\.?\d*)(k|m|b|%)?(?:\s|$|[^a-z0-9%])`)

func extractThreshold(norm string) string {
	matches := numberPattern.FindAllStringSubmatch(norm, -1)
	if len(matches) == 0 {
		return ""
	}

	// Find the most "interesting" number — skip years (2024-2030) and small numbers
	for _, m := range matches {
		numStr := strings.ReplaceAll(m[1], ",", "")
		suffix := m[2]

		// Try to parse as a number
		var val float64
		n, err := fmt.Sscanf(numStr, "%f", &val)
		if err != nil || n != 1 {
			continue
		}

		// Apply suffix
		switch suffix {
		case "k":
			val *= 1000
		case "m":
			val *= 1000000
		case "b":
			val *= 1000000000
		case "%":
			// Keep as percentage
			return fmt.Sprintf("%.1f%%", val)
		}

		// Skip years (2020-2035)
		if val >= 2020 && val <= 2035 && suffix == "" {
			continue
		}

		// Return normalized number
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%.1f", val)
	}

	return ""
}

// dateRefPattern matches year references and month+year patterns
var dateRefPattern = regexp.MustCompile(`(?:(?:january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|oct|nov|dec)\s+)?(20\d{2})`)
var monthPattern = regexp.MustCompile(`(january|february|march|april|may|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|oct|nov|dec)\s+(20\d{2})`)

func extractDateRef(norm string) string {
	// Try month+year first (more specific)
	if m := monthPattern.FindStringSubmatch(norm); m != nil {
		month := normalizeMonth(m[1])
		return month + " " + m[2]
	}
	// Fall back to just year
	if m := dateRefPattern.FindStringSubmatch(norm); m != nil {
		return m[len(m)-1]
	}
	return ""
}

func normalizeMonth(m string) string {
	months := map[string]string{
		"january": "jan", "february": "feb", "march": "mar",
		"april": "apr", "may": "may", "june": "jun",
		"july": "jul", "august": "aug", "september": "sep",
		"october": "oct", "november": "nov", "december": "dec",
		"jan": "jan", "feb": "feb", "mar": "mar", "apr": "apr",
		"jun": "jun", "jul": "jul", "aug": "aug", "sep": "sep",
		"oct": "oct", "nov": "nov", "dec": "dec",
	}
	if n, ok := months[m]; ok {
		return n
	}
	return m
}

// --- Entity normalization ---

// entitySynonyms maps entity fragments to their canonical form.
// This resolves cases where "fed" and "reserve" should both become "federal reserve".
var entitySynonyms = map[string]string{
	"fed":              "federal reserve",
	"fomc":             "federal reserve",
	"reserve":          "federal reserve",
	"btc":              "bitcoin",
	"eth":              "ethereum",
	"sol":              "solana",
	"doge":             "dogecoin",
	"ada":              "cardano",
	"xrp":              "ripple",
	"gop":              "republican",
	"dem":              "democrat",
	"dems":             "democrat",
	"potus":            "president",
	"scotus":           "supreme court",
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

// --- Metric extraction ---

// metricChecks are checked in priority order — more specific patterns first.
var metricChecks = []struct {
	metric   string
	patterns []string
}{
	{"rate", []string{"rate", "interest rate", "fed funds", "basis points", "bps"}},
	{"market_cap", []string{"market cap", "marketcap", "mcap", "valuation"}},
	{"votes", []string{"vote", "votes", "electoral", "polling", "poll"}},
	{"count", []string{"number of", "count", "total", "how many"}},
	{"percentage", []string{"percent", "percentage", "%", "share of", "approval rating"}},
	{"index", []string{"index", "s&p", "nasdaq", "dow jones", "vix"}},
	{"price", []string{"price", "trading", "trade at", "worth", "valued"}},
}

// cryptoTerms — when a crypto entity appears with a numeric threshold,
// the metric is almost certainly "price".
var cryptoTerms = []string{"bitcoin", "ethereum", "solana", "dogecoin", "btc", "eth", "crypto"}

func extractMetric(norm string) string {
	// Check specific metric patterns in priority order
	for _, mc := range metricChecks {
		for _, p := range mc.patterns {
			if strings.Contains(norm, p) {
				return mc.metric
			}
		}
	}

	// Infer price from crypto entity + numeric threshold
	for _, term := range cryptoTerms {
		if strings.Contains(norm, term) {
			if numberPattern.MatchString(norm) {
				return "price"
			}
		}
	}

	// Infer price from $ symbol + threshold
	if strings.Contains(norm, "$") && numberPattern.MatchString(norm) {
		return "price"
	}

	return ""
}

// --- Comparator extraction ---

// comparatorMap maps action+direction combinations to canonical comparator names.
var comparatorMap = map[string]string{
	"reach|above":  "reach",
	"hit|above":    "reach",
	"exceed|above": "exceed",
	"reach|":       "reach",
	"hit|":         "reach",
	"exceed|":      "exceed",
	"drop|below":   "drop_below",
	"fall|below":   "drop_below",
	"fall|":        "drop_below",
	"drop|":        "drop_below",
	"rise|above":   "rise_above",
	"rise|":        "rise_above",
	"win|":         "win",
	"elect|":       "win",
	"cut|":         "cut",
	"raise|":       "raise",
	"pass|":        "pass",
	"approve|":     "approve",
	"ban|":         "ban",
	"sign|":        "sign",
	"impeach|":     "impeach",
	"resign|":      "resign",
	"default|":     "default",
}

func extractComparator(norm, action, direction string) string {
	// Try action+direction combination first
	key := action + "|" + direction
	if comp, ok := comparatorMap[key]; ok {
		return comp
	}
	// Try action-only
	key = action + "|"
	if comp, ok := comparatorMap[key]; ok {
		return comp
	}

	// Pattern-based fallback
	if strings.Contains(norm, "stay above") || strings.Contains(norm, "remain above") {
		return "stay_above"
	}
	if strings.Contains(norm, "stay below") || strings.Contains(norm, "remain below") {
		return "stay_below"
	}
	if strings.Contains(norm, "end above") || strings.Contains(norm, "close above") {
		return "close_above"
	}
	if strings.Contains(norm, "end below") || strings.Contains(norm, "close below") {
		return "close_below"
	}

	return ""
}

// --- Region extraction ---

var regionPatterns = map[string][]string{
	"us":     {"united states", "u.s.", "us ", "american", "federal reserve", "fed ", "congress", "senate", "potus"},
	"uk":     {"united kingdom", "u.k.", "uk ", "british", "bank of england", "parliament"},
	"eu":     {"european", "europe", "eu ", "ecb", "eurozone"},
	"china":  {"china", "chinese", "beijing", "pboc"},
	"global": {"global", "world", "worldwide", "international"},
}

func extractRegion(norm string) string {
	for region, patterns := range regionPatterns {
		for _, p := range patterns {
			if strings.Contains(norm, p) {
				return region
			}
		}
	}
	return ""
}

// --- Market type detection ---

func detectMarketType(norm string) string {
	// Range markets: "between X and Y", "in the range"
	if strings.Contains(norm, "between") && strings.Contains(norm, "and") {
		return "range"
	}
	if strings.Contains(norm, "in the range") {
		return "range"
	}
	// Multi-outcome: "who will win", "which"
	if strings.HasPrefix(norm, "who ") || strings.HasPrefix(norm, "which ") {
		return "multi"
	}
	return "binary"
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
