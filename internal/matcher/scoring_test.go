package matcher

import (
	"math"
	"testing"
)

// ─── Levenshtein ────────────────────────────────────────────────────────────

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"empty_both", "", "", 0},
		{"empty_a", "", "abc", 3},
		{"empty_b", "abc", "", 3},
		{"identical", "hello", "hello", 0},
		{"single_char_same", "a", "a", 0},
		{"single_char_diff", "a", "b", 1},
		{"insertion", "abc", "abcd", 1},
		{"deletion", "abcd", "abc", 1},
		{"substitution", "abc", "adc", 1},
		{"known_distance_kitten_sitting", "kitten", "sitting", 3},
		{"known_distance_saturday_sunday", "saturday", "sunday", 3},
		{"completely_different", "abc", "xyz", 3},
		{"transposition", "ab", "ba", 2}, // levenshtein does not count transposition as 1
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := levenshtein(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// ─── EditSimilarity ─────────────────────────────────────────────────────────

func TestEditSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want float64
		tol  float64
	}{
		{"identical", "hello", "hello", 1.0, 0.001},
		{"empty_both", "", "", 1.0, 0.001},
		{"one_empty", "hello", "", 0.0, 0.001},
		{"one_char_diff", "abc", "adc", 0.667, 0.01},
		{"completely_different", "abc", "xyz", 0.0, 0.001},
		{"kitten_sitting", "kitten", "sitting", 0.571, 0.01}, // 1 - 3/7
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := editSimilarity(tt.a, tt.b)
			if math.Abs(got-tt.want) > tt.tol {
				t.Errorf("editSimilarity(%q, %q) = %.3f, want %.3f (tol=%.3f)",
					tt.a, tt.b, got, tt.want, tt.tol)
			}
		})
	}
}

// ─── KeywordJaccard ─────────────────────────────────────────────────────────

func TestKeywordJaccard(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want float64
		tol  float64
	}{
		{"empty_both", "", "", 1.0, 0.001},
		{"identical_keywords", "bitcoin reach 100000", "bitcoin reach 100000", 1.0, 0.001},
		{"completely_disjoint", "bitcoin crypto", "trump election", 0.0, 0.001},
		{"partial_overlap", "bitcoin reach 100000 2026", "bitcoin hit 100000", 0.40, 0.15}, // 2 shared out of ~5 union
		{"stopwords_only", "will the be in on", "to by at or and", 1.0, 0.001},             // all filtered, both empty → 1.0
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// keywordJaccard operates on already-normalized strings
			got := keywordJaccard(tt.a, tt.b)
			if math.Abs(got-tt.want) > tt.tol {
				t.Errorf("keywordJaccard(%q, %q) = %.3f, want %.3f (tol=%.3f)",
					tt.a, tt.b, got, tt.want, tt.tol)
			}
		})
	}
}

// ─── NormTitle ──────────────────────────────────────────────────────────────

func TestNormTitle(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(string) bool
		desc  string
	}{
		{
			"lowercase",
			"Will BITCOIN Hit $100K?",
			func(s string) bool { return s == normTitle("will bitcoin hit $100k?") },
			"should be case-insensitive",
		},
		{
			"strips_punctuation",
			"What's the deal?!",
			func(s string) bool {
				for _, r := range s {
					if r == '?' || r == '!' || r == '\'' {
						return false
					}
				}
				return true
			},
			"should strip question marks, exclamations, apostrophes",
		},
		{
			"normalizes_numbers_100k",
			"bitcoin 100k",
			func(s string) bool {
				return containsWord(s, "100000")
			},
			"should expand 100k to 100000",
		},
		{
			"applies_synonyms_btc",
			"btc price",
			func(s string) bool {
				return containsWord(s, "bitcoin")
			},
			"should convert btc to bitcoin via synonyms",
		},
		{
			"applies_synonyms_fed",
			"fed rate cut",
			func(s string) bool {
				return containsWord(s, "federal") && containsWord(s, "reserve")
			},
			"should convert fed to federal reserve via synonyms",
		},
		{
			"collapses_whitespace",
			"  lots   of   spaces  ",
			func(s string) bool {
				for i := 0; i < len(s)-1; i++ {
					if s[i] == ' ' && s[i+1] == ' ' {
						return false
					}
				}
				return true
			},
			"should collapse multiple spaces",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normTitle(tt.input)
			if !tt.check(got) {
				t.Errorf("normTitle(%q) = %q: %s", tt.input, got, tt.desc)
			}
		})
	}
}

func containsWord(s, word string) bool {
	for _, w := range splitWords(s) {
		if w == word {
			return true
		}
	}
	return false
}

func splitWords(s string) []string {
	var words []string
	start := -1
	for i, r := range s {
		if r == ' ' {
			if start >= 0 {
				words = append(words, s[start:i])
				start = -1
			}
		} else {
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		words = append(words, s[start:])
	}
	return words
}

// ─── NormalizeNumbers ───────────────────────────────────────────────────────

func TestNormalizeNumbers(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"100k", "100k", "100000"},
		{"1.5m", "1.5m", "1500000"},
		{"2.5b", "2.5b", "2500000000"},
		{"plain_number", "5000", "5000"},
		{"no_number", "hello world", "hello world"},
		{"mixed", "bitcoin 100k by 2026", "bitcoin 100000 by 2026"},
		{"50k", "50k", "50000"},
		{"10m", "10m", "10000000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeNumbers(tt.input)
			if got != tt.want {
				t.Errorf("normalizeNumbers(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ─── EntityMismatchPenalty ──────────────────────────────────────────────────

func TestEntityMismatchPenalty(t *testing.T) {
	tests := []struct {
		name       string
		a, b       string
		jaccardSim float64
		wantZero   bool // true means we expect penalty == 0
	}{
		{
			"identical_no_penalty",
			"bitcoin reach 100000 2026",
			"bitcoin reach 100000 2026",
			1.0,
			true,
		},
		{
			"low_jaccard_no_penalty",
			"bitcoin crypto moon",
			"trump election politics",
			0.0,
			true,
		},
		{
			"template_mismatch_same_structure",
			"lakers win nba championship 2026",
			"celtics win nba championship 2026",
			0.75,
			false, // same template, different entity → penalty
		},
		{
			"same_entity",
			"bitcoin reach 100000",
			"bitcoin hit 100000",
			0.67,
			false, // "reach" vs "hit" are different words; template ratio is high so penalty applies
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := entityMismatchPenalty(tt.a, tt.b, tt.jaccardSim)
			if tt.wantZero && got != 0 {
				t.Errorf("entityMismatchPenalty = %.2f, want 0 for %q vs %q", got, tt.a, tt.b)
			}
			if !tt.wantZero && got == 0 {
				t.Errorf("entityMismatchPenalty = 0, expected > 0 for %q vs %q", tt.a, tt.b)
			}
		})
	}
}

// ─── DifferentActions ───────────────────────────────────────────────────────

func TestDifferentActions(t *testing.T) {
	tests := []struct {
		name    string
		titleA  string
		titleB  string
		wantDif bool
	}{
		{"win_vs_win", "Trump wins the election", "Trump wins 2028", false},
		{"win_vs_impeach", "Trump wins the election", "Trump impeached before 2028", true},
		{"cut_vs_raise", "Fed cuts rates", "Fed raises rates", true},
		{"reach_vs_hit", "Bitcoin reaches $100k", "Bitcoin hits $100k", true}, // extractAction returns different verbs ("reach" vs "hit")
		{"no_action_a", "The weather tomorrow", "Trump wins", false},           // no action in A
		{"no_action_b", "Trump wins", "The weather tomorrow", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := differentActions(tt.titleA, tt.titleB)
			if got != tt.wantDif {
				t.Errorf("differentActions(%q, %q) = %v, want %v", tt.titleA, tt.titleB, got, tt.wantDif)
			}
		})
	}
}

// ─── FuzzyTitleScore (pinned scores) ────────────────────────────────────────

func TestFuzzyTitleScorePinned(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		min  float64
		max  float64
	}{
		{
			"identical",
			"Will Bitcoin reach $100,000 by end of 2026?",
			"Will Bitcoin reach $100,000 by end of 2026?",
			0.95, 1.0,
		},
		{
			"synonym_btc_bitcoin",
			"Will Bitcoin hit $100,000 in 2026?",
			"Will BTC reach $100k by 2026?",
			0.50, 0.95,
		},
		{
			"completely_unrelated",
			"Will Bitcoin reach $100,000?",
			"Will Trump win the 2028 election?",
			0.0, 0.30,
		},
		{
			"same_entity_different_action",
			"Trump wins the 2028 election",
			"Trump impeached before 2028",
			0.10, 0.50,
		},
		{
			"fed_synonym",
			"Fed cuts interest rates in 2026",
			"Federal Reserve rate cut before 2027",
			0.20, 0.70,
		},
		{
			"same_template_different_entity",
			"Lakers win NBA Championship 2026",
			"Celtics win NBA Championship 2026",
			0.30, 0.75,
		},
		{
			"eth_synonym",
			"Will Ethereum reach $5,000?",
			"ETH above $5000",
			0.15, 0.95, // synonyms normalize ETH→ethereum and above→reach, so very similar after normalization
		},
		{
			"similar_but_different_index",
			"S&P 500 above 6000",
			"Nasdaq above 6000",
			0.10, 0.55,
		},
		{
			"inflation_same_question",
			"Will inflation be above 3% in June?",
			"Will inflation exceed 3% by June?",
			0.40, 1.0, // "above"→"reach" vs "exceed" causes some divergence in edit distance
		},
		{
			"long_vs_short_title",
			"Will the United States Federal Reserve Board of Governors cut the federal funds rate by at least 25 basis points before June 30 2026?",
			"Fed rate cut 2026",
			0.05, 0.40,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fuzzyTitleScore(tt.a, tt.b)
			if got < tt.min || got > tt.max {
				t.Errorf("fuzzyTitleScore = %.3f, want [%.2f, %.2f]\n  a: %q\n  b: %q\n  normA: %q\n  normB: %q",
					got, tt.min, tt.max, tt.a, tt.b, normTitle(tt.a), normTitle(tt.b))
			}
		})
	}
}

// ─── DisjointSpecificEntities ───────────────────────────────────────────────

func TestDisjointSpecificEntities(t *testing.T) {
	tests := []struct {
		name    string
		titleA  string
		titleB  string
		want    bool
	}{
		{"same_entity", "Trump wins 2028", "Trump wins election", false},
		{"different_entities", "Trump wins 2028", "Harris wins 2028", true},
		{"no_entities_a", "will rates go up", "Harris wins", false},
		{"no_entities_b", "Trump wins", "will rates go up", false},
		{"overlapping_entities", "Trump and Harris debate", "Trump wins election", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := disjointSpecificEntities(tt.titleA, tt.titleB)
			if got != tt.want {
				t.Errorf("disjointSpecificEntities(%q, %q) = %v, want %v",
					tt.titleA, tt.titleB, got, tt.want)
			}
		})
	}
}
