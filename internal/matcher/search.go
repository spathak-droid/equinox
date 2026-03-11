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

// FindEquivalentPairsFromSearch uses the LLM to match search candidates across venues.
//
// Pre-filter pipeline (batched, with fallback expansion):
//  1. Rank all markets per venue by query-word match score.
//  2. Try the top batchSize per venue. If no matches found, slide to the next
//     batch of batchSize and retry — continuing until matches are found or
//     all markets are exhausted.
//  3. Within each batch, selectCandidates re-ranks kalshi by Jaccard per poly.
const batchSize = 10

func (m *Matcher) FindEquivalentPairsFromSearch(ctx context.Context, searchResults []SearchResult, query string) []*MatchResult {
	// Collect unique markets per venue
	polyMap := map[string]*models.CanonicalMarket{}
	kalshiMap := map[string]*models.CanonicalMarket{}
	for _, sr := range searchResults {
		addToVenueMap(sr.Source, polyMap, kalshiMap)
		for _, c := range sr.Candidates {
			addToVenueMap(c, polyMap, kalshiMap)
		}
	}

	var allPoly, allKalshi []*models.CanonicalMarket
	for _, mm := range polyMap {
		allPoly = append(allPoly, mm)
	}
	for _, mm := range kalshiMap {
		allKalshi = append(allKalshi, mm)
	}

	// Rank all markets by query-word match score (descending)
	rankedPoly := topByQueryMatch(query, allPoly, len(allPoly))
	rankedKalshi := topByQueryMatch(query, allKalshi, len(allKalshi))

	fmt.Printf("[matcher/search] Ranked pools: poly=%d kalshi=%d\n", len(rankedPoly), len(rankedKalshi))

	if len(rankedPoly) == 0 || len(rankedKalshi) == 0 {
		fmt.Printf("[matcher/search] No cross-venue pairs to compare\n")
		return nil
	}

	llm := NewLLMMatcher()
	if llm == nil {
		fmt.Printf("[matcher/search] WARNING: OPENAI_API_KEY not set, falling back to fuzzy matching\n")
		return m.fuzzyFallback(ctx, searchResults)
	}

	// Try batches until we find matches or exhaust the list
	maxOffset := len(rankedPoly)
	if len(rankedKalshi) > maxOffset {
		maxOffset = len(rankedKalshi)
	}

	// Try batches of batchSize from the ranked lists. Within each batch,
	// pre-rank all pairs by Jaccard and send only the top 10 to the LLM.
	var llmResults []*LLMMatchResult
	for offset := 0; offset < maxOffset; offset += batchSize {
		var polyBatch, kalshiBatch []*models.CanonicalMarket
		if offset < len(rankedPoly) {
			end := offset + batchSize
			if end > len(rankedPoly) {
				end = len(rankedPoly)
			}
			polyBatch = rankedPoly[offset:end]
		}
		if offset < len(rankedKalshi) {
			end := offset + batchSize
			if end > len(rankedKalshi) {
				end = len(rankedKalshi)
			}
			kalshiBatch = rankedKalshi[offset:end]
		}

		if len(polyBatch) == 0 && len(kalshiBatch) == 0 {
			break
		}

		top10 := topPairsByJaccard(polyBatch, kalshiBatch, 10)
		fmt.Printf("[matcher/search] offset=%d: top-%d pairs by Jaccard → LLM\n", offset, len(top10))

		batchResults := llm.MatchPairs(ctx, top10)
		if len(batchResults) > 0 {
			llmResults = batchResults
			fmt.Printf("[matcher/search] Found %d results at offset=%d\n", len(batchResults), offset)
			break
		}
	}

	// Final fallback: query match exhausted — pick top 10 pairs from all markets
	// by Jaccard similarity and send only those to the LLM.
	if len(llmResults) == 0 {
		top10 := topPairsByJaccard(allPoly, allKalshi, 10)
		fmt.Printf("[matcher/search] Full cross-pollination fallback: top-%d pairs → LLM\n", len(top10))
		llmResults = llm.MatchPairs(ctx, top10)
	}

	// Convert LLM results to MatchResults, cross-validating with rule-based scores
	var confirmed []*MatchResult
	for _, lr := range llmResults {
		// Run rule-based comparison to get real fuzzy/semantic scores
		ruleResult := m.compare(lr.MarketA, lr.MarketB)

		// Cross-validation: if the LLM says match but rule-based scores are very low,
		// the LLM is probably wrong (same topic, different specific question).
		llmConf := lr.Confidence
		realFuzzy := ruleResult.FuzzyScore
		realEntity := ruleResult.EntityOverlapScore
		if realEntity < 0 {
			realEntity = 0
		}

		// Compute effective confidence: blend LLM with rule-based signals
		// If fuzzy < 0.3 and entity overlap < 0.4, the titles are clearly different
		// questions — override the LLM regardless of its confidence.
		effectiveConf := llmConf
		if realFuzzy < 0.30 && realEntity < 0.40 {
			effectiveConf = 0.0 // hard reject: titles too different
			fmt.Printf("[matcher/search] VETOED by rules: LLM=%.2f but fuzzy=%.2f entity=%.2f | %q vs %q\n",
				llmConf, realFuzzy, realEntity, lr.MarketA.Title, lr.MarketB.Title)
		} else if realFuzzy < 0.45 {
			// Moderate penalty: reduce LLM confidence proportionally
			effectiveConf = llmConf * (realFuzzy / 0.45)
			fmt.Printf("[matcher/search] Downgraded: LLM=%.2f → %.2f (fuzzy=%.2f) | %q vs %q\n",
				llmConf, effectiveConf, realFuzzy, lr.MarketA.Title, lr.MarketB.Title)
		}

		// Guardrails: prevent same-topic-but-different-question false positives.
		// We require at least ONE strong signal beyond the LLM:
		//  - rule composite clears probable threshold, OR
		//  - strong entity overlap (shared key entities), OR
		//  - signature match, OR
		//  - strong event match score
		hasRuleSupport := ruleResult.CompositeScore >= m.cfg.ProbableMatchThreshold
		hasEntitySupport := realEntity >= 0.40
		hasEventSupport := ruleResult.EventMatchScore >= 0.60
		hasSignature := ruleResult.SignatureMatch

		if !hasRuleSupport && !hasEntitySupport && !hasEventSupport && !hasSignature {
			fmt.Printf("[matcher/search] Rejected by composite gate: llm=%.2f rule=%.2f entity=%.2f event=%.2f | %q vs %q\n",
				llmConf, ruleResult.CompositeScore, realEntity, ruleResult.EventMatchScore, lr.MarketA.Title, lr.MarketB.Title)
			continue
		}

		confidence := ConfidenceNoMatch
		if effectiveConf >= 0.8 {
			confidence = ConfidenceMatch
		} else if effectiveConf >= 0.5 {
			confidence = ConfidenceProbable
		}

		result := &MatchResult{
			MarketA:             lr.MarketA,
			MarketB:             lr.MarketB,
			Confidence:          confidence,
			CompositeScore:      effectiveConf,
			FuzzyScore:          realFuzzy,
			EmbeddingScore:      -1,
			EventMatchScore:     ruleResult.EventMatchScore,
			EntityOverlapScore:  realEntity,
			DateProximityScore:  ruleResult.DateProximityScore,
			PriceProximityScore: ruleResult.PriceProximityScore,
			Explanation:         fmt.Sprintf("LLM confidence=%.2f (effective=%.2f): %s", llmConf, effectiveConf, lr.Reasoning),
		}

		if confidence != ConfidenceNoMatch {
			confirmed = append(confirmed, result)
			fmt.Printf("[matcher/search] %s — %s vs %s (eff=%.2f, fuzzy=%.2f): %s | %s\n",
				confidence, lr.MarketA.VenueID, lr.MarketB.VenueID, effectiveConf,
				realFuzzy, lr.MarketA.Title, lr.Reasoning)
		}
	}

	// Dedup (same pair found from both directions)
	confirmed = DeduplicatePairs(confirmed)

	return confirmed
}

// fuzzyFallback uses the old rule-based matching when no LLM is available.
func (m *Matcher) fuzzyFallback(ctx context.Context, searchResults []SearchResult) []*MatchResult {
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

	fmt.Printf("[matcher/search] Fuzzy fallback: %d poly × %d kalshi\n", len(polyMarkets), len(kalshiMarkets))

	// For each poly market, find its best kalshi match by Jaccard title similarity.
	// This mirrors the query-based pre-filter but without a query.
	var confirmed []*MatchResult
	for _, poly := range polyMarkets {
		// Pick best kalshi candidate by Jaccard, then run full rule-based compare
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
// Despite the legacy name, selection uses full rule-based compare() (semantic
// signature, hard gates, fuzzy score, date checks), not raw token overlap.
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
