package news

import (
	"strings"

	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
)

// BuildNewsQuery constructs a search query for finding relevant news about a
// matched market pair. Strategy (priority order):
//  1. Extract entities + metric from EventSignature
//  2. Keyword intersection of both titles
//  3. Fallback: cleaned MarketA title
func BuildNewsQuery(a, b *models.CanonicalMarket) string {
	// Strategy 1: Use extracted event signature entities + metric
	sigA := matcher.ExtractEventSignature(a.Title)
	if len(sigA.Entities) > 0 {
		parts := make([]string, len(sigA.Entities))
		copy(parts, sigA.Entities)
		if sigA.Metric != "" {
			parts = append(parts, sigA.Metric)
		}
		q := strings.Join(parts, " ")
		if len(strings.Fields(q)) <= 8 {
			return q
		}
		// Too long — truncate to first 6 words
		return truncateWords(q, 6)
	}

	// Strategy 2: Keyword intersection of both titles
	wordsA := significantWords(a.Title)
	wordsB := significantWords(b.Title)
	var shared []string
	for _, w := range wordsA {
		for _, w2 := range wordsB {
			if w == w2 {
				shared = append(shared, w)
				break
			}
		}
	}
	if len(shared) >= 2 {
		return truncateWords(strings.Join(shared, " "), 8)
	}

	// Strategy 3: Fallback to cleaned MarketA title
	return truncateWords(cleanTitle(a.Title), 6)
}

// significantWords returns lowercased non-stopword tokens from a title.
func significantWords(title string) []string {
	stops := map[string]bool{
		"will": true, "the": true, "a": true, "an": true, "be": true,
		"is": true, "in": true, "on": true, "of": true, "to": true,
		"by": true, "at": true, "or": true, "and": true, "for": true,
		"this": true, "that": true, "with": true, "from": true,
		"what": true, "does": true, "do": true, "can": true,
		"before": true, "after": true, "end": true, "year": true,
	}
	var out []string
	for _, w := range strings.Fields(strings.ToLower(title)) {
		// Strip punctuation
		w = strings.TrimFunc(w, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
		})
		if len(w) > 1 && !stops[w] {
			out = append(out, w)
		}
	}
	return out
}

// cleanTitle strips common prediction market prefixes and suffixes.
func cleanTitle(title string) string {
	t := strings.ToLower(title)
	t = strings.TrimPrefix(t, "will ")
	t = strings.TrimSuffix(t, "?")
	t = strings.TrimSpace(t)
	return t
}

// truncateWords limits a string to at most n words.
func truncateWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) <= n {
		return s
	}
	return strings.Join(words[:n], " ")
}
