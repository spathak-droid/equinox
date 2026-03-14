// Package matcher — event_extract.go contains extraction helpers for parsing
// structured fields (action, direction, threshold, date, metric, comparator,
// region, market type) from market titles.
package matcher

import (
	"fmt"
	"regexp"
	"strings"
)

// --- Action extraction ---

// actionVerbs are verbs that indicate what is being predicted.
var actionVerbs = []string{
	"win", "reach", "hit", "exceed", "drop", "fall", "rise",
	"cut", "raise", "elect", "impeach", "resign", "announce",
	"approve", "ban", "pass", "sign", "launch", "default",
	"invade", "attack", "print", "close", "open", "trade",
	"compete", "qualify", "skip", "boycott", "host", "attend",
	"play", "lead", "pull", "remove", "fire", "hire", "appoint",
	"score", "assist", "transfer", "buy", "sell", "acquire",
	"merge", "nationalize", "privatize", "endorse", "nominate",
	"veto", "override", "ratify", "repeal", "withdraw", "deploy",
	"meet", "visit", "say", "post", "tweet", "pardon", "convict",
	"indict", "arrest", "extradite", "sanction", "recognize",
	"annex", "add", "reduce", "increase", "break", "set",
}

// actionIntentGroups clusters action verbs into intent categories.
// Two verbs in the same group are considered compatible (asking the same kind of question).
// Two verbs in different groups are considered incompatible (different questions).
var actionIntentGroups = map[string]string{
	// Outcome/victory
	"win": "outcome", "elect": "outcome", "champion": "outcome",
	// Participation/presence
	"play": "participate", "compete": "participate", "attend": "participate",
	"qualify": "participate", "skip": "non-participate", "boycott": "non-participate",
	// Hosting/location
	"host": "location", "visit": "location", "meet": "location",
	// Removal/withdrawal
	"pull": "removal", "remove": "removal", "fire": "removal", "withdraw": "removal",
	"impeach": "removal", "resign": "removal",
	// Performance metrics
	"lead": "metric", "score": "metric", "assist": "metric",
	// Price movement
	"reach": "price_move", "hit": "price_move", "exceed": "price_move",
	"drop": "price_move", "fall": "price_move", "rise": "price_move",
	"close": "price_move", "open": "price_move", "trade": "price_move",
	"break": "price_move", "set": "price_move",
	// Policy actions
	"cut": "policy", "raise": "policy",
	"approve": "policy", "ban": "policy", "pass": "policy",
	"sign": "policy", "veto": "policy", "override": "policy",
	"ratify": "policy", "repeal": "policy",
	"nationalize": "policy", "privatize": "policy",
	"sanction": "policy", "recognize": "policy",
	// Business actions
	"launch": "business", "acquire": "business", "merge": "business",
	"buy": "business", "sell": "business",
	// Communication
	"announce": "communicate", "say": "communicate", "post": "communicate",
	"tweet": "communicate", "endorse": "communicate", "nominate": "communicate",
	// Legal
	"convict": "legal", "indict": "legal", "arrest": "legal",
	"extradite": "legal", "pardon": "legal",
	// Military
	"invade": "military", "attack": "military", "deploy": "military",
	"annex": "military",
	// Hiring
	"hire": "hiring", "appoint": "hiring",
	// Quantitative change
	"reduce": "quantity", "increase": "quantity", "add": "quantity",
	// Default
	"default": "default",
	"transfer": "transfer",
	"print": "print",
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

// --- Direction extraction ---

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

// --- Threshold extraction ---

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

// --- Date reference extraction ---

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
	"qualify|":     "qualify",
	"compete|":     "compete",
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
