package matcher

import (
	"strings"
	"unicode"
)

// entityStopwords are capitalized words that appear frequently in prediction market
// titles but are NOT named entities. These are political terms, sports terms, and
// other common words that happen to be capitalized.
var entityStopwords = map[string]bool{
	"democratic": true, "republican": true, "presidential": true,
	"president": true, "presidency": true, "senator": true,
	"governor": true, "congress": true, "senate": true,
	"house": true, "party": true, "primary": true,
	"nomination": true, "nominee": true, "election": true,
	"championship": true, "finals": true, "final": true, "conference": true,
	"league": true, "cup": true, "division": true,
	"qualifiers": true, "qualifier": true, "semifinal": true, "semifinals": true,
	"eastern": true, "western": true, "northern": true, "southern": true,
	"united": true, "states": true, "america": true,
	"january": true, "february": true, "march": true, "april": true,
	"may": true, "june": true, "july": true, "august": true,
	"september": true, "october": true, "november": true, "december": true,
}

// extractEntities returns lowercased proper nouns from the original (un-normalized) title.
// A word is considered a named entity if it starts with an uppercase letter, is not
// the first word in the title (sentence-start), and is not a common non-entity word.
func extractEntities(title string) []string {
	words := strings.Fields(title)
	var entities []string
	for i, w := range words {
		if len(w) == 0 {
			continue
		}
		first := rune(w[0])
		if !unicode.IsUpper(first) {
			continue
		}
		// Skip first word only when it's a question lead word ("Will", "Can", ...).
		// Keep true entities that appear first (e.g., "Iran to compete ...").
		if i == 0 {
			lead := strings.ToLower(strings.TrimFunc(w, func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsDigit(r)
			}))
			switch lead {
			case "will", "can", "is", "are", "was", "were", "do", "does", "did",
				"has", "have", "had", "should", "could", "would",
				"who", "what", "when", "where", "why", "how":
				continue
			}
		}
		// Clean punctuation and lowercase for matching
		cleaned := strings.ToLower(strings.TrimFunc(w, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}))
		if len(cleaned) <= 1 {
			continue
		}
		// Skip common capitalized non-entity words
		if entityStopwords[cleaned] {
			continue
		}
		entities = append(entities, cleaned)
	}
	return entities
}

// entityOverlapScore computes the Jaccard similarity of named entity sets
// extracted from two market titles. Returns a value in [0.0, 1.0].
func entityOverlapScore(titleA, titleB string) float64 {
	entA := extractEntities(titleA)
	entB := extractEntities(titleB)

	setA := make(map[string]bool, len(entA))
	for _, e := range entA {
		setA[e] = true
	}
	setB := make(map[string]bool, len(entB))
	for _, e := range entB {
		setB[e] = true
	}

	if len(setA) == 0 && len(setB) == 0 {
		return 0.5 // neutral when no entities found
	}
	if len(setA) == 0 || len(setB) == 0 {
		return 0.0
	}

	intersection := 0
	for e := range setA {
		if setB[e] {
			intersection++
		}
	}

	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0.0
	}
	return float64(intersection) / float64(union)
}
