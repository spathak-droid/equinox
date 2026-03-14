// Package matcher -- index.go implements fast in-memory inverted-index matching.
//
// Instead of calling venue search APIs (slow, rate-limited HTTP round-trips),
// this builds an in-memory inverted index on market title keywords and finds
// candidate pairs that share meaningful keywords. Combined with multi-signal
// scoring (fuzzy + entities + dates + prices + category), this resolves
// matches without any AI/embedding dependency.
package matcher

import (
	"context"
	"fmt"
	"sort"

	"github.com/equinox/internal/models"
)

// MarketIndex is an in-memory inverted index mapping keywords to market IDs.
type MarketIndex struct {
	markets  map[string]*models.CanonicalMarket // venueMarketID -> market
	inverted map[string][]string                // keyword -> list of venueMarketIDs
}

// BuildIndex creates an inverted index from a slice of canonical markets.
// Each market is indexed by its meaningful keywords (stopwords removed) plus
// named entities extracted from the original title.
func BuildIndex(markets []*models.CanonicalMarket) *MarketIndex {
	idx := &MarketIndex{
		markets:  make(map[string]*models.CanonicalMarket, len(markets)),
		inverted: make(map[string][]string),
	}

	for _, m := range markets {
		idx.markets[m.VenueMarketID] = m

		// Extract keywords from normalized title (existing stopword removal)
		kw := keywords(normTitle(m.Title))

		// Also extract named entities (proper nouns from the original title)
		for _, e := range extractEntities(m.Title) {
			kw[e] = true
		}

		for word := range kw {
			idx.inverted[word] = append(idx.inverted[word], m.VenueMarketID)
		}
	}

	// IDF filtering: remove keywords that appear in >10% of markets.
	// These are too common to be discriminative (e.g. "2026", "price")
	// and create massive candidate lists at scale.
	threshold := len(markets) / 10
	if threshold < 50 {
		threshold = 50
	}
	removed := 0
	for word, ids := range idx.inverted {
		if len(ids) > threshold {
			delete(idx.inverted, word)
			removed++
		}
	}
	if removed > 0 {
		fmt.Printf("[matcher/index] Removed %d high-frequency keywords (>%d markets)\n", removed, threshold)
	}

	return idx
}

// FindCandidates returns markets from DIFFERENT venues that share at least
// minSharedKeywords keywords with the input market, sorted by shared count descending.
func (idx *MarketIndex) FindCandidates(market *models.CanonicalMarket, minSharedKeywords int) []*models.CanonicalMarket {
	// Get this market's keywords + entities
	kw := keywords(normTitle(market.Title))
	for _, e := range extractEntities(market.Title) {
		kw[e] = true
	}

	// Count shared keywords per candidate
	counts := map[string]int{}
	for word := range kw {
		for _, id := range idx.inverted[word] {
			if id == market.VenueMarketID {
				continue
			}
			counts[id]++
		}
	}

	// Filter: different venue + minimum shared keywords
	type candidate struct {
		market *models.CanonicalMarket
		shared int
	}
	var candidates []candidate
	for id, count := range counts {
		if count < minSharedKeywords {
			continue
		}
		m := idx.markets[id]
		if m.VenueID == market.VenueID {
			continue
		}
		candidates = append(candidates, candidate{market: m, shared: count})
	}

	// Sort by shared keyword count descending
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].shared > candidates[j].shared })

	result := make([]*models.CanonicalMarket, len(candidates))
	for i, c := range candidates {
		result[i] = c.market
	}
	return result
}

// FindSignatureMatches performs Stage 0 batch matching: extract signatures from
// all markets and find cross-venue pairs with identical canonical signatures.
// This runs in O(n) time and produces instant high-confidence matches.
func FindSignatureMatches(markets []*models.CanonicalMarket) []*MatchResult {
	// Build signature -> markets index
	sigIndex := map[string][]*models.CanonicalMarket{}
	for _, m := range markets {
		sig := ExtractEventSignature(m.Title)
		cs := sig.CanonicalSignature()
		if cs == "" {
			continue
		}
		m.SemanticSignature = cs
		sigIndex[cs] = append(sigIndex[cs], m)
	}

	var results []*MatchResult
	for sig, group := range sigIndex {
		if len(group) < 2 {
			continue
		}
		// Find cross-venue pairs within this signature group
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				a, b := group[i], group[j]
				if a.VenueID == b.VenueID {
					continue
				}
				sigA := ExtractEventSignature(a.Title)
				results = append(results, &MatchResult{
					MarketA:         a,
					MarketB:         b,
					Confidence:      ConfidenceMatch,
					CompositeScore:  1.0,
					FuzzyScore:      fuzzyTitleScore(a.Title, b.Title),
					EventMatchScore: 1.0,
					SignatureMatch:  true,
					Explanation: fmt.Sprintf(
						"Stage 0 signature match: sig=%s (entities=%v, threshold=%s, date=%s)",
						sig, sigA.Entities, sigA.Threshold, sigA.DateRef),
				})
			}
		}
	}

	fmt.Printf("[matcher/sig] Signature pre-pass: %d markets -> %d signatures -> %d instant matches\n",
		len(markets), len(sigIndex), len(results))
	return results
}

// FindEquivalentPairsFromIndex uses the inverted index to find and score
// equivalent market pairs without any API calls or AI dependencies.
//
// Pipeline:
//  1. Build inverted index from all markets
//  2. For each market, find candidates sharing >=2 keywords (from different venues)
//  3. Score each candidate pair using multi-signal composite
//  4. Apply vote-based disambiguation for PROBABLE pairs
//  5. Deduplicate and sort results
func (m *Matcher) FindEquivalentPairsFromIndex(ctx context.Context, markets []*models.CanonicalMarket) []*MatchResult {
	fmt.Println("[matcher/index] Building inverted index...")
	idx := BuildIndex(markets)
	fmt.Printf("[matcher/index] Indexed %d markets with %d unique keywords\n",
		len(idx.markets), len(idx.inverted))

	// Adaptive minimum shared keywords: require more overlap for large datasets
	minShared := 2
	if len(markets) > 5000 {
		minShared = 3
	}

	// Track pairs we've already compared to avoid duplicates
	type pairKey struct{ a, b string }
	seen := map[pairKey]bool{}

	var confirmed []*MatchResult
	var ambiguous []*MatchResult
	candidatePairs := 0

	for _, market := range markets {
		candidates := idx.FindCandidates(market, minShared)
		for _, candidate := range candidates {
			// Deduplicate: ensure we only compare each pair once
			k := pairKey{a: market.VenueMarketID, b: candidate.VenueMarketID}
			if market.VenueMarketID > candidate.VenueMarketID {
				k = pairKey{a: candidate.VenueMarketID, b: market.VenueMarketID}
			}
			if seen[k] {
				continue
			}
			seen[k] = true
			candidatePairs++

			result := m.compare(market, candidate)
			switch result.Confidence {
			case ConfidenceMatch:
				confirmed = append(confirmed, result)
			case ConfidenceProbable:
				ambiguous = append(ambiguous, result)
			}
		}
	}

	fmt.Printf("[matcher/index] Scored %d candidate pairs: %d confirmed, %d ambiguous\n",
		candidatePairs, len(confirmed), len(ambiguous))

	// Vote-based disambiguation for ambiguous pairs (replaces LLM)
	if len(ambiguous) > 0 {
		upgraded := 0
		downgraded := 0
		for _, r := range ambiguous {
			verdict := voteBasedDisambiguation(r)
			switch verdict {
			case ConfidenceMatch:
				r.Confidence = ConfidenceMatch
				r.Explanation = fmt.Sprintf(
					"Vote-upgraded to MATCH: fuzzy=%.2f, entity=%.2f, date=%.2f, price=%.2f, cat=%s (composite=%.2f)",
					r.FuzzyScore, r.EntityOverlapScore, r.DateProximityScore,
					r.PriceProximityScore, categoryLabel(r.MarketA, r.MarketB), r.CompositeScore)
				confirmed = append(confirmed, r)
				upgraded++
			case ConfidenceNoMatch:
				r.Confidence = ConfidenceNoMatch
				downgraded++
			default:
				// Remains PROBABLE -- keep if composite is above match threshold
				if r.CompositeScore >= m.cfg.MatchThreshold {
					confirmed = append(confirmed, r)
				}
			}
		}
		fmt.Printf("[matcher/index] Vote disambiguation: %d upgraded, %d downgraded, %d kept as probable\n",
			upgraded, downgraded, len(ambiguous)-upgraded-downgraded)
	}

	// Sort by composite score descending
	sort.Slice(confirmed, func(i, j int) bool { return confirmed[i].CompositeScore > confirmed[j].CompositeScore })

	for _, r := range confirmed {
		fmt.Printf("[matcher/index] %s -- %s vs %s (score=%.3f): %s\n",
			r.Confidence, r.MarketA.VenueID, r.MarketB.VenueID, r.CompositeScore, r.Explanation)
	}

	return confirmed
}

// ClusterMatchResult holds match results organized by topic cluster.
type ClusterMatchResult struct {
	Cluster *TopicCluster
	Pairs   []*MatchResult
}

// FindEquivalentPairsFromClusters implements the full pipeline:
// cluster by topic -> match within clusters -> deduplicate -> sort.
//
// This is the primary matching strategy for broad ingestion. It groups
// markets into topic buckets first, then only compares markets within
// each cross-venue cluster, dramatically reducing comparisons.
func (m *Matcher) FindEquivalentPairsFromClusters(ctx context.Context, markets []*models.CanonicalMarket) ([]*MatchResult, []*ClusterMatchResult) {
	clusters := ClusterByTopic(markets)

	crossVenue := 0
	for _, c := range clusters {
		if c.HasCrossVenue() {
			crossVenue++
		}
	}
	fmt.Printf("[matcher/cluster] %d total clusters, %d cross-venue\n", len(clusters), crossVenue)

	// Track all pairs globally to deduplicate across clusters
	type pairKey struct{ a, b string }
	globalSeen := map[pairKey]bool{}

	var allConfirmed []*MatchResult
	var clusterResults []*ClusterMatchResult

	for _, cluster := range clusters {
		if !cluster.HasCrossVenue() {
			continue
		}

		// Run the index-based matcher within this cluster
		var confirmed []*MatchResult
		var ambiguous []*MatchResult

		idx := BuildIndex(cluster.Markets)
		minShared := 1 // lower threshold within clusters since they're already topically related
		if len(cluster.Markets) > 200 {
			minShared = 2
		}

		seen := map[pairKey]bool{}
		candidatePairs := 0

		for _, market := range cluster.Markets {
			candidates := idx.FindCandidates(market, minShared)
			for _, candidate := range candidates {
				k := pairKey{a: market.VenueMarketID, b: candidate.VenueMarketID}
				if market.VenueMarketID > candidate.VenueMarketID {
					k = pairKey{a: candidate.VenueMarketID, b: market.VenueMarketID}
				}
				if seen[k] || globalSeen[k] {
					continue
				}
				seen[k] = true
				candidatePairs++

				result := m.compare(market, candidate)
				switch result.Confidence {
				case ConfidenceMatch:
					confirmed = append(confirmed, result)
				case ConfidenceProbable:
					ambiguous = append(ambiguous, result)
				}
			}
		}

		// Also do direct cross-venue comparison for small clusters
		// (the index might miss pairs with only 1 shared keyword)
		if len(cluster.Markets) <= 50 {
			for i := 0; i < len(cluster.Markets); i++ {
				for j := i + 1; j < len(cluster.Markets); j++ {
					a, b := cluster.Markets[i], cluster.Markets[j]
					if a.VenueID == b.VenueID {
						continue
					}
					k := pairKey{a: a.VenueMarketID, b: b.VenueMarketID}
					if a.VenueMarketID > b.VenueMarketID {
						k = pairKey{a: b.VenueMarketID, b: a.VenueMarketID}
					}
					if seen[k] || globalSeen[k] {
						continue
					}
					seen[k] = true
					candidatePairs++

					result := m.compare(a, b)
					switch result.Confidence {
					case ConfidenceMatch:
						confirmed = append(confirmed, result)
					case ConfidenceProbable:
						ambiguous = append(ambiguous, result)
					}
				}
			}
		}

		// Vote-based disambiguation for ambiguous pairs
		for _, r := range ambiguous {
			verdict := voteBasedDisambiguation(r)
			switch verdict {
			case ConfidenceMatch:
				r.Confidence = ConfidenceMatch
				r.Explanation = fmt.Sprintf(
					"Vote-upgraded to MATCH (cluster=%q): fuzzy=%.2f, entity=%.2f, date=%.2f, price=%.2f, composite=%.2f",
					cluster.Label, r.FuzzyScore, r.EntityOverlapScore, r.DateProximityScore,
					r.PriceProximityScore, r.CompositeScore)
				confirmed = append(confirmed, r)
			case ConfidenceNoMatch:
				// drop
			default:
				if r.CompositeScore >= m.cfg.MatchThreshold {
					confirmed = append(confirmed, r)
				}
			}
		}

		// Mark all confirmed pairs as globally seen
		for _, r := range confirmed {
			k := pairKey{a: r.MarketA.VenueMarketID, b: r.MarketB.VenueMarketID}
			if r.MarketA.VenueMarketID > r.MarketB.VenueMarketID {
				k = pairKey{a: r.MarketB.VenueMarketID, b: r.MarketA.VenueMarketID}
			}
			globalSeen[k] = true
		}

		if len(confirmed) > 0 {
			fmt.Printf("[matcher/cluster] Cluster %q: %d markets, %d candidates, %d matches\n",
				cluster.Label, len(cluster.Markets), candidatePairs, len(confirmed))
			clusterResults = append(clusterResults, &ClusterMatchResult{
				Cluster: cluster,
				Pairs:   confirmed,
			})
			allConfirmed = append(allConfirmed, confirmed...)
		}
	}

	// Sort all results by composite score descending
	sort.SliceStable(allConfirmed, func(i, j int) bool {
		return allConfirmed[i].CompositeScore > allConfirmed[j].CompositeScore
	})

	fmt.Printf("[matcher/cluster] Total: %d matches across %d clusters\n",
		len(allConfirmed), len(clusterResults))

	return allConfirmed, clusterResults
}
