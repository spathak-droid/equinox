// Package matcher — search.go implements query-based cross-venue matching.
//
// Instead of the brute-force O(n²) approach (compare every market from venue A
// against every market from venue B), this uses each venue's search API to find
// candidate matches for each market, then scores only those candidates.
//
// This reduces comparisons from ~250,000 (500×500) to ~3,000 (500×5 candidates)
// and focuses on markets that actually overlap topically.
package matcher

import (
	"context"
	"fmt"

	"github.com/equinox/internal/models"
)

// SearchCandidate pairs a source market with candidate matches found via search.
type SearchCandidate struct {
	Source    *models.CanonicalMarket
	Candidate *models.CanonicalMarket
}

// SearchResult holds the output of a cross-search: source markets from one venue
// paired with candidate matches found via the other venue's search API.
type SearchResult struct {
	Source     *models.CanonicalMarket
	Candidates []*models.CanonicalMarket
}

// FindEquivalentPairsFromSearch uses rule-based matching on search candidates.
//
// Pipeline:
//  1. Collect all unique markets per venue from search results.
//  2. For each poly market, find its best kalshi match by Jaccard title similarity.
//  3. Run full rule-based compare (semantic signature, hard gates, fuzzy score, date checks).
//  4. Keep pairs that clear the match or probable-match threshold.
func (m *Matcher) FindEquivalentPairsFromSearch(_ context.Context, searchResults []SearchResult, _ string) []*MatchResult {
	// Collect all unique markets per venue
	polyMap := map[string]*models.CanonicalMarket{}
	kalshiMap := map[string]*models.CanonicalMarket{}
	for _, sr := range searchResults {
		addToVenueMap(sr.Source, polyMap, kalshiMap)
		for _, c := range sr.Candidates {
			addToVenueMap(c, polyMap, kalshiMap)
		}
	}
	var polyMarkets, kalshiMarkets []*models.CanonicalMarket
	for _, mm := range polyMap {
		polyMarkets = append(polyMarkets, mm)
	}
	for _, mm := range kalshiMap {
		kalshiMarkets = append(kalshiMarkets, mm)
	}

	fmt.Printf("[matcher/search] Rule-based matching: %d poly × %d kalshi\n", len(polyMarkets), len(kalshiMarkets))

	if len(polyMarkets) == 0 || len(kalshiMarkets) == 0 {
		fmt.Printf("[matcher/search] No cross-venue pairs to compare\n")
		return nil
	}

	// For each poly market, find its best kalshi match by Jaccard title similarity,
	// then run full rule-based compare.
	var confirmed []*MatchResult
	for _, poly := range polyMarkets {
		var bestK *models.CanonicalMarket
		bestSim := -1.0
		for _, k := range kalshiMarkets {
			if sim := tokenJaccard(poly.Title, k.Title); sim > bestSim {
				bestSim = sim
				bestK = k
			}
		}
		if bestK == nil {
			continue
		}
		result := m.compare(poly, bestK)
		if result.Confidence == ConfidenceMatch ||
			(result.Confidence == ConfidenceProbable && result.CompositeScore >= m.cfg.ProbableMatchThreshold) {
			confirmed = append(confirmed, result)
		}
	}

	confirmed = DeduplicatePairs(confirmed)

	// Sort by composite score descending so best pairs win dedup.
	for i := 1; i < len(confirmed); i++ {
		for j := i; j > 0 && confirmed[j].CompositeScore > confirmed[j-1].CompositeScore; j-- {
			confirmed[j], confirmed[j-1] = confirmed[j-1], confirmed[j]
		}
	}

	// Each market may appear in at most one pair — keep the highest-scoring match.
	confirmed = deduplicateByMarket(confirmed)

	return confirmed
}

// CrossPollinateJaccard finds cross-venue pairs from broad pools for UI queries.
// Selection uses full rule-based compare() (semantic signature, hard gates,
// fuzzy score, date checks), not raw token overlap.
func (m *Matcher) CrossPollinateJaccard(polyMarkets, kalshiMarkets []*models.CanonicalMarket) []*MatchResult {
	if len(polyMarkets) == 0 || len(kalshiMarkets) == 0 {
		return nil
	}

	// Score all cross-venue candidates, then deduplicate globally by market.
	// This avoids missing good pairs when a market's local "best" candidate
	// conflicts with a stronger global pairing.
	var candidates []*MatchResult
	for _, poly := range polyMarkets {
		for _, k := range kalshiMarkets {
			r := m.compare(poly, k)
			if r.Confidence == ConfidenceNoMatch {
				continue
			}
			// Keep PROBABLE matches that clear the probable threshold.
			if r.Confidence == ConfidenceProbable && r.CompositeScore < m.cfg.ProbableMatchThreshold {
				continue
			}
			candidates = append(candidates, r)
		}
	}

	// Sort by score so dedup keeps strongest pairs first.
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].CompositeScore > candidates[j-1].CompositeScore; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	results := deduplicateByMarket(candidates)

	fmt.Printf("[matcher] CrossPollinateJaccard: %d poly × %d kalshi → %d pairs\n",
		len(polyMarkets), len(kalshiMarkets), len(results))
	return results
}
