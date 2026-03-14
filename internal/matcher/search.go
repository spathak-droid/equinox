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
	// Collect all unique markets grouped by venue
	venueMarkets := groupByVenue(searchResults)

	venueIDs := make([]models.VenueID, 0, len(venueMarkets))
	for vid := range venueMarkets {
		venueIDs = append(venueIDs, vid)
	}

	if len(venueIDs) < 2 {
		fmt.Printf("[matcher/search] No cross-venue pairs to compare (found %d venues)\n", len(venueIDs))
		return nil
	}

	// Log venue counts
	for _, vid := range venueIDs {
		fmt.Printf("[matcher/search] Venue %s: %d markets\n", vid, len(venueMarkets[vid]))
	}

	// For each market in venue A, find its best match in venue B by Jaccard title similarity,
	// then run full rule-based compare. Compare across all venue pairs.
	var confirmed []*MatchResult
	for i := 0; i < len(venueIDs); i++ {
		for j := i + 1; j < len(venueIDs); j++ {
			marketsA := venueMarkets[venueIDs[i]]
			marketsB := venueMarkets[venueIDs[j]]
			fmt.Printf("[matcher/search] Rule-based matching: %d (%s) × %d (%s)\n",
				len(marketsA), venueIDs[i], len(marketsB), venueIDs[j])
			for _, a := range marketsA {
				var bestB *models.CanonicalMarket
				bestSim := -1.0
				for _, b := range marketsB {
					if sim := tokenJaccard(a.Title, b.Title); sim > bestSim {
						bestSim = sim
						bestB = b
					}
				}
				if bestB == nil {
					continue
				}
				result := m.compare(a, bestB)
				if result.Confidence == ConfidenceMatch ||
					(result.Confidence == ConfidenceProbable && result.CompositeScore >= m.cfg.ProbableMatchThreshold) {
					confirmed = append(confirmed, result)
				}
			}
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

// FindMatchesFromSearchResults compares each source market against its top-N
// candidates (not just the single best Jaccard match). This is used when
// candidates are pre-selected by FTS search from the SQLite index.
func (m *Matcher) FindMatchesFromSearchResults(results []SearchResult, topN int) []*MatchResult {
	if topN <= 0 {
		topN = 5
	}

	var confirmed []*MatchResult
	compared := 0

	for _, sr := range results {
		if sr.Source == nil || len(sr.Candidates) == 0 {
			continue
		}

		// Limit candidates per source market
		candidates := sr.Candidates
		if len(candidates) > topN {
			candidates = candidates[:topN]
		}

		for _, cand := range candidates {
			if sr.Source.VenueID == cand.VenueID {
				continue // same venue, skip
			}
			result := m.compare(sr.Source, cand)
			compared++
			if result.Confidence == ConfidenceMatch ||
				(result.Confidence == ConfidenceProbable && result.CompositeScore >= m.cfg.ProbableMatchThreshold) {
				confirmed = append(confirmed, result)
			}
		}
	}

	fmt.Printf("[matcher/index] Compared %d pairs from %d search results → %d candidates\n",
		compared, len(results), len(confirmed))

	confirmed = DeduplicatePairs(confirmed)

	// Sort by composite score descending
	for i := 1; i < len(confirmed); i++ {
		for j := i; j > 0 && confirmed[j].CompositeScore > confirmed[j-1].CompositeScore; j-- {
			confirmed[j], confirmed[j-1] = confirmed[j-1], confirmed[j]
		}
	}

	confirmed = deduplicateByMarket(confirmed)

	return confirmed
}

// FindMatchesRelaxed is like FindMatchesFromSearchResults but uses a lower
// composite score threshold. This is for Qdrant-sourced results where semantic
// similarity has already been established by vector search.
func (m *Matcher) FindMatchesRelaxed(results []SearchResult, topN int, minScore float64) []*MatchResult {
	if topN <= 0 {
		topN = 5
	}

	var confirmed []*MatchResult
	compared := 0

	for _, sr := range results {
		if sr.Source == nil || len(sr.Candidates) == 0 {
			continue
		}
		candidates := sr.Candidates
		if len(candidates) > topN {
			candidates = candidates[:topN]
		}

		for _, cand := range candidates {
			if sr.Source.VenueID == cand.VenueID {
				continue
			}
			result := m.compareLight(sr.Source, cand)
			compared++
			if result.CompositeScore >= minScore {
				confirmed = append(confirmed, result)
			}
		}
	}

	fmt.Printf("[matcher/qdrant] Compared %d pairs from %d search results → %d candidates (min=%.2f)\n",
		compared, len(results), len(confirmed), minScore)

	confirmed = DeduplicatePairs(confirmed)

	for i := 1; i < len(confirmed); i++ {
		for j := i; j > 0 && confirmed[j].CompositeScore > confirmed[j-1].CompositeScore; j-- {
			confirmed[j], confirmed[j-1] = confirmed[j-1], confirmed[j]
		}
	}

	confirmed = deduplicateByMarket(confirmed)
	return confirmed
}

// CrossPollinateJaccard finds cross-venue pairs using event-level matching.
// Markets are grouped into events first, then events are matched across venues.
// Within matched events, child markets are paired by outcome similarity.
func (m *Matcher) CrossPollinateJaccard(marketsA, marketsB []*models.CanonicalMarket) []*MatchResult {
	if len(marketsA) == 0 || len(marketsB) == 0 {
		return nil
	}

	// Group markets into events
	eventsA := models.GroupByEvent(marketsA)
	eventsB := models.GroupByEvent(marketsB)
	fmt.Printf("[matcher] Event-level matching: %d events (venue A) × %d events (venue B)\n",
		len(eventsA), len(eventsB))

	// Match events, then pair child markets within matched events
	eventResults := m.MatchEvents(eventsA, eventsB)

	for _, er := range eventResults {
		fmt.Printf("[matcher] %s event: %q ≈ %q (score=%.3f, %d market pairs)\n",
			er.Confidence, er.EventA.EventTitle, er.EventB.EventTitle,
			er.Score, len(er.MarketPairs))
	}

	results := FlattenEventMatches(eventResults)

	fmt.Printf("[matcher] CrossPollinateJaccard: %d events (venue A) × %d events (venue B) → %d event matches → %d market pairs\n",
		len(eventsA), len(eventsB), len(eventResults), len(results))
	return results
}
