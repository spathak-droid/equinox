package kalshisearch

import (
	"math"
	"strings"
)

// ScoreCandidate is a pure scoring function and can be unit-tested independently.
// It returns (score, matched) where matched indicates whether query text matched.
func ScoreCandidate(item ResultItem, query string) (float64, bool) {
	q := normalizeText(query)
	ticker := strings.ToUpper(strings.TrimSpace(item.Ticker))
	title := normalizeText(item.Title)
	subtitle := normalizeText(item.Subtitle)
	eventTicker := strings.ToUpper(strings.TrimSpace(item.EventTicker))
	seriesTicker := strings.ToUpper(strings.TrimSpace(item.SeriesTicker))
	queryUpper := strings.ToUpper(strings.TrimSpace(query))

	score := 0.0
	matched := false

	if q != "" {
		if queryUpper != "" && ticker == queryUpper {
			score += 100
			matched = true
		}
		if queryUpper != "" && strings.Contains(ticker, queryUpper) {
			score += 40
			matched = true
		}
		if strings.Contains(title, q) {
			score += 30
			matched = true
		}
		if strings.Contains(subtitle, q) {
			score += 15
			matched = true
		}
		if queryUpper != "" && (strings.Contains(eventTicker, queryUpper) || strings.Contains(seriesTicker, queryUpper)) {
			score += 20
			matched = true
		}

		for _, tok := range strings.Fields(q) {
			if tok == "" {
				continue
			}
			if strings.Contains(title, tok) {
				score += 2
				matched = true
			}
			if strings.Contains(subtitle, tok) {
				score += 1
				matched = true
			}
		}
	}

	if isOpenStatus(item.Status) {
		score += 10
	}

	// Small tie-breaker bonus: keep bounded and low impact.
	score += math.Min(5.0, math.Log10(1.0+item.Liquidity))
	score += math.Min(5.0, math.Log10(1.0+item.Volume))

	return score, matched
}

func normalizeText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	return strings.Join(strings.Fields(s), " ")
}

func isOpenStatus(status string) bool {
	st := strings.ToLower(strings.TrimSpace(status))
	return st == "open" || st == "active"
}

