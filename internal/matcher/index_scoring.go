package matcher

import (
	"math"

	"github.com/equinox/internal/models"
)

// dateProximityScore returns a positive signal [0.0, 1.0] based on how close
// two markets' resolution dates are.
//   - Both within 30 days: 1.0
//   - Linear decay to 0.0 at maxDateDeltaDays
//   - Either missing a date: 0.5 (neutral)
func dateProximityScore(a, b *models.CanonicalMarket, maxDateDeltaDays int) float64 {
	if !a.HasResolutionDate() || !b.HasResolutionDate() {
		return 0.5
	}

	delta := a.ResolutionDate.Sub(*b.ResolutionDate)
	if delta < 0 {
		delta = -delta
	}
	deltaDays := delta.Hours() / 24

	if deltaDays <= 30 {
		return 1.0
	}
	maxDays := float64(maxDateDeltaDays)
	if deltaDays >= maxDays {
		return 0.0
	}
	// Linear decay from 1.0 at 30 days to 0.0 at maxDays
	return 1.0 - (deltaDays-30)/(maxDays-30)
}

// priceProximityScore returns [0.0, 1.0] based on how close two markets'
// YES prices are. Only meaningful when both prices are non-zero.
func priceProximityScore(a, b *models.CanonicalMarket) float64 {
	if a.YesPrice == 0 && b.YesPrice == 0 {
		return 0.5 // neutral -- no price data
	}
	if a.YesPrice == 0 || b.YesPrice == 0 {
		return 0.5 // neutral -- partial data
	}
	return 1.0 - math.Abs(a.YesPrice-b.YesPrice)
}

// categoryBonus returns a score adjustment based on category alignment.
//   - Same category: +0.15
//   - Different non-"other" categories: -0.10
//   - One or both "other": 0.0 (neutral)
func categoryBonus(a, b *models.CanonicalMarket) float64 {
	catA := a.Category
	catB := b.Category
	if catA == "" {
		catA = "other"
	}
	if catB == "" {
		catB = "other"
	}

	if catA == catB && catA != "other" {
		return 0.15
	}
	if catA != "other" && catB != "other" && catA != catB {
		return -0.10
	}
	return 0.0
}

// categoryLabel returns a human-readable category comparison label.
func categoryLabel(a, b *models.CanonicalMarket) string {
	catA := a.Category
	catB := b.Category
	if catA == "" {
		catA = "other"
	}
	if catB == "" {
		catB = "other"
	}
	if catA == catB {
		return catA
	}
	return catA + "/" + catB
}

// voteBasedDisambiguation resolves ambiguous pairs using a vote-based approach.
// A pair is upgraded to MATCH if >=3 of 5 conditions are true.
// Downgraded to NO_MATCH if <2 votes.
//
// Special guard: if entity overlap is very low (<0.20) AND keyword Jaccard is high,
// this is a template mismatch (same race, different person) -- force NO_MATCH regardless
// of other votes, since date/price/category all correlate within the same event.
func voteBasedDisambiguation(result *MatchResult) MatchConfidence {
	// Template mismatch veto: when both titles have entities but overlap is low,
	// these are different subjects in the same event (e.g. different candidates)
	if result.EntityOverlapScore >= 0 && result.EntityOverlapScore < 0.40 {
		entA := extractEntities(result.MarketA.Title)
		entB := extractEntities(result.MarketB.Title)
		if len(entA) > 0 && len(entB) > 0 {
			return ConfidenceNoMatch
		}
	}

	votes := 0

	if result.FuzzyScore >= 0.50 {
		votes++
	}
	if result.EntityOverlapScore >= 0.60 {
		votes++
	}
	if result.DateProximityScore >= 0.80 {
		votes++
	}
	if result.PriceProximityScore >= 0.85 {
		votes++
	}
	// Category match
	catA := result.MarketA.Category
	catB := result.MarketB.Category
	if catA == "" {
		catA = "other"
	}
	if catB == "" {
		catB = "other"
	}
	if catA == catB && catA != "other" {
		votes++
	}

	if votes >= 3 {
		return ConfidenceMatch
	}
	if votes < 2 {
		return ConfidenceNoMatch
	}
	return ConfidenceProbable
}
