package matcher

import (
	"fmt"
	"math"
	"strings"
)

// ─── Fuzzy title scoring ──────────────────────────────────────────────────────

// fuzzyTitleScore returns a [0.0, 1.0] similarity score for two market titles.
func fuzzyTitleScore(a, b string) float64 {
	na, nb := normTitle(a), normTitle(b)

	editSim := editSimilarity(na, nb)
	jaccardSim := keywordJaccard(na, nb)

	base := 0.5*editSim + 0.5*jaccardSim

	// Entity mismatch penalty
	penalty := entityMismatchPenalty(na, nb, jaccardSim)

	score := base - penalty
	if score < 0 {
		score = 0
	}
	return score
}

// entityMismatchPenalty detects when two titles share a template but differ in
// the subject entity.
func entityMismatchPenalty(a, b string, jaccardSim float64) float64 {
	if jaccardSim < 0.5 || a == b {
		return 0
	}

	wordsA := strings.Fields(a)
	wordsB := strings.Fields(b)

	setA := map[string]bool{}
	setB := map[string]bool{}
	for _, w := range wordsA {
		setA[w] = true
	}
	for _, w := range wordsB {
		setB[w] = true
	}

	var onlyA, onlyB []string
	for w := range setA {
		if !setB[w] {
			onlyA = append(onlyA, w)
		}
	}
	for w := range setB {
		if !setA[w] {
			onlyB = append(onlyB, w)
		}
	}

	if len(onlyA) > 0 && len(onlyB) > 0 {
		shared := 0
		for w := range setA {
			if setB[w] {
				shared++
			}
		}
		totalUnique := len(onlyA) + len(onlyB)
		if shared > 0 && totalUnique > 0 {
			templateRatio := float64(shared) / float64(shared+totalUnique)
			if templateRatio > 0.6 {
				return 0.25
			}
			if templateRatio > 0.4 {
				return 0.15
			}
		}
	}

	return 0
}

// normTitle lowercases, strips punctuation, normalizes numbers and synonyms for comparison.
func normTitle(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, ch := range s {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == ' ' || ch == '.' {
			b.WriteRune(ch)
		}
	}
	result := strings.Join(strings.Fields(b.String()), " ")
	result = normalizeNumbers(result)
	result = applySynonyms(result)
	return result
}

// normalizeNumbers converts shorthand like "100k", "1.5m", "2.5b" to plain integers.
func normalizeNumbers(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = normalizeNumberWord(w)
	}
	return strings.Join(words, " ")
}

func normalizeNumberWord(w string) string {
	if len(w) == 0 {
		return w
	}
	last := w[len(w)-1]
	var multiplier float64
	switch last {
	case 'k':
		multiplier = 1_000
	case 'm':
		multiplier = 1_000_000
	case 'b':
		multiplier = 1_000_000_000
	default:
		return w
	}
	numPart := w[:len(w)-1]
	var val float64
	n, err := fmt.Sscanf(numPart, "%f", &val)
	if err != nil || n != 1 {
		return w
	}
	return fmt.Sprintf("%d", int64(val*multiplier))
}

// synonyms maps common prediction-market terms to canonical forms.
var synonyms = map[string]string{
	"btc": "bitcoin", "eth": "ethereum", "xrp": "ripple",
	"sol": "solana", "doge": "dogecoin", "ada": "cardano",
	"gop": "republican", "dems": "democrat", "dem": "democrat",
	"potus": "president", "scotus": "supreme court",
	"fed": "federal reserve", "fomc": "federal reserve",
	"gdp": "gross domestic product", "cpi": "consumer price index",
	"pce": "personal consumption expenditures", "bps": "basis points",
	"reaches": "reach", "reached": "reach", "reaching": "reach",
	"wins": "win", "winning": "win", "won": "win",
	"hits": "hit", "hitting": "hit",
	"drops": "drop", "dropping": "drop", "dropped": "drop",
	"rises": "rise", "rising": "rise",
	"falls": "fall", "falling": "fall",
	"cuts": "cut", "cutting": "cut",
	"raises": "raise", "raising": "raise",
	"exceeds": "exceed", "exceeding": "exceed", "exceeded": "exceed",
	"impeached": "impeach", "impeaching": "impeach",
	"resigned": "resign", "resigning": "resign",
	"elected": "elect", "electing": "elect",
	"approved": "approve", "approving": "approve",
	"banned": "ban", "banning": "ban",
	"passed": "pass", "passing": "pass",
	"signed": "sign", "signing": "sign",
	"launched": "launch", "launching": "launch",
	"defaulted": "default", "defaulting": "default",
	"rates": "rate", "prices": "price", "elections": "election",
	"above": "reach", "below": "under",
	"higher": "above", "lower": "below",
	"over": "above", "greater": "above", "less": "below",
}

func applySynonyms(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if canonical, ok := synonyms[w]; ok {
			words[i] = canonical
		}
	}
	return strings.Join(words, " ")
}

// editSimilarity returns 1 - normalizedLevenshtein(a, b).
func editSimilarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	d := levenshtein(a, b)
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 1.0
	}
	return 1.0 - float64(d)/float64(maxLen)
}

// keywordJaccard returns the Jaccard similarity of keyword sets after removing stopwords.
func keywordJaccard(a, b string) float64 {
	setA := keywords(a)
	setB := keywords(b)

	if len(setA) == 0 && len(setB) == 0 {
		return 1.0
	}

	intersection := 0
	for w := range setA {
		if setB[w] {
			intersection++
		}
	}

	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// keywords returns the set of meaningful words in a title, removing stopwords.
func keywords(s string) map[string]bool {
	stops := map[string]bool{
		"will": true, "the": true, "a": true, "an": true, "be": true,
		"is": true, "in": true, "on": true, "of": true, "to": true,
		"by": true, "at": true, "or": true, "and": true, "for": true,
		"get": true, "have": true, "has": true, "its": true,
		"this": true, "that": true, "with": true, "from": true,
		"what": true, "does": true, "do": true, "can": true,
		"end": true, "year": true, "before": true, "after": true,
	}
	set := map[string]bool{}
	for _, w := range strings.Fields(s) {
		if !stops[w] && len(w) > 1 {
			set[w] = true
		}
	}
	return set
}

// levenshtein computes the Levenshtein edit distance between two strings.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)

	dp := make([][]int, la+1)
	for i := range dp {
		dp[i] = make([]int, lb+1)
	}
	for i := 0; i <= la; i++ {
		dp[i][0] = i
	}
	for j := 0; j <= lb; j++ {
		dp[0][j] = j
	}
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			if ra[i-1] == rb[j-1] {
				dp[i][j] = dp[i-1][j-1]
			} else {
				dp[i][j] = 1 + min3(dp[i-1][j], dp[i][j-1], dp[i-1][j-1])
			}
		}
	}
	return dp[la][lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

var genericEntityTokens = map[string]bool{
	"fifa": true, "world": true, "cup": true, "game": true, "games": true,
	"olympics": true, "election": true, "presidential": true, "candidate": true,
	"market": true, "price": true, "year": true, "yes": true, "no": true,
}

// disjointSpecificEntities returns true when both titles contain specific named
// entities but share none of them after removing generic template tokens.
// Entity synonyms (e.g. "fed" -> "federal reserve") are resolved before comparison
// so that "Fed" and "Federal Reserve" are recognized as the same entity.
func disjointSpecificEntities(aTitle, bTitle string) bool {
	aRaw := extractEntities(aTitle)
	bRaw := extractEntities(bTitle)
	if len(aRaw) == 0 || len(bRaw) == 0 {
		return false
	}

	// Resolve synonyms so "fed" and "federal reserve" overlap correctly.
	aNorm := normalizeEntities(aRaw)
	bNorm := normalizeEntities(bRaw)

	aSet := map[string]bool{}
	for _, e := range aNorm {
		if !genericEntityTokens[e] {
			aSet[e] = true
		}
	}
	bSet := map[string]bool{}
	for _, e := range bNorm {
		if !genericEntityTokens[e] {
			bSet[e] = true
		}
	}
	if len(aSet) == 0 || len(bSet) == 0 {
		return false
	}
	for e := range aSet {
		if bSet[e] {
			return false
		}
	}
	return true
}

// cosineSimilarity computes the cosine similarity between two float32 vectors.
// Retained as a utility for potential future use.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
