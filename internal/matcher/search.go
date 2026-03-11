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
	"strings"
	"sync"
	"time"

	"github.com/equinox/internal/models"
)

// SearchCandidate pairs a source market with candidate matches found via search.
type SearchCandidate struct {
	Source     *models.CanonicalMarket
	Candidate *models.CanonicalMarket
}

// SearchQueryExtractor returns the best search query for a market.
// For Kalshi markets with composite titles ("Event — Subtitle"), we use
// the event title since that's the actual topic. For everything else,
// we use the market title as-is and let the search APIs handle relevance.
func SearchQueryExtractor(m *models.CanonicalMarket) string {
	// If we have a venue event title (e.g. Kalshi's event_title), prefer it
	// as the search query — it's the clean topic without market-specific suffixes.
	if m.VenueEventTitle != "" {
		return strings.TrimSpace(m.VenueEventTitle)
	}
	return strings.TrimSpace(m.Title)
}

// DeduplicatePairs removes duplicate match results (same market pair regardless of order).
func DeduplicatePairs(pairs []*MatchResult) []*MatchResult {
	type pairKey struct {
		a, b string // VenueMarketIDs, sorted
	}
	seen := map[pairKey]bool{}
	var out []*MatchResult

	for _, p := range pairs {
		idA := p.MarketA.VenueMarketID
		idB := p.MarketB.VenueMarketID
		// Normalize key order
		k := pairKey{a: idA, b: idB}
		if idA > idB {
			k = pairKey{a: idB, b: idA}
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	return out
}

// SearchResult holds the output of a cross-search: source markets from one venue
// paired with candidate matches found via the other venue's search API.
type SearchResult struct {
	Source     *models.CanonicalMarket
	Candidates []*models.CanonicalMarket
}

// deduplicateSearchResults collapses duplicate source→candidate pairs before scoring.
// When multiple source markets (e.g. 10 Polymarket Bitcoin markets) each find the same
// Kalshi candidate via search, we only need to score each unique (source, candidate) pair
// once. We keep the source with the highest fuzzy title similarity to the candidate,
// since that's the most meaningful comparison.
func deduplicateSearchResults(searchResults []SearchResult) []SearchResult {
	// First, collect all unique candidates across all search results
	type candidateKey struct {
		sourceVenue    models.VenueID
		candidateID    string
	}

	// Group: for each unique candidate, find the best source market to pair it with
	type bestPair struct {
		source    *models.CanonicalMarket
		candidate *models.CanonicalMarket
		fuzzy     float64
	}
	best := map[candidateKey]*bestPair{}

	for _, sr := range searchResults {
		for _, candidate := range sr.Candidates {
			if sr.Source.VenueID == candidate.VenueID {
				continue
			}
			key := candidateKey{
				sourceVenue: sr.Source.VenueID,
				candidateID: candidate.VenueMarketID,
			}
			score := fuzzyTitleScore(sr.Source.Title, candidate.Title)
			if existing, ok := best[key]; !ok || score > existing.fuzzy {
				best[key] = &bestPair{
					source:    sr.Source,
					candidate: candidate,
					fuzzy:     score,
				}
			}
		}
	}

	// Also deduplicate candidates themselves (same market returned by different queries)
	seenCandidates := map[string]*models.CanonicalMarket{}
	for _, bp := range best {
		if existing, ok := seenCandidates[bp.candidate.VenueMarketID]; ok {
			// Keep the one with more data (higher liquidity as proxy)
			if bp.candidate.Liquidity > existing.Liquidity {
				seenCandidates[bp.candidate.VenueMarketID] = bp.candidate
			}
		} else {
			seenCandidates[bp.candidate.VenueMarketID] = bp.candidate
		}
	}

	// Rebuild deduplicated search results: one SearchResult per unique (source, candidate)
	var out []SearchResult
	seen := map[string]bool{}
	for _, bp := range best {
		// Use the canonical version of the candidate
		bp.candidate = seenCandidates[bp.candidate.VenueMarketID]

		pairKey := bp.source.VenueMarketID + "|" + bp.candidate.VenueMarketID
		if seen[pairKey] {
			continue
		}
		seen[pairKey] = true

		// Check if this source already has a SearchResult
		found := false
		for i := range out {
			if out[i].Source.VenueMarketID == bp.source.VenueMarketID {
				out[i].Candidates = append(out[i].Candidates, bp.candidate)
				found = true
				break
			}
		}
		if !found {
			out = append(out, SearchResult{
				Source:     bp.source,
				Candidates: []*models.CanonicalMarket{bp.candidate},
			})
		}
	}

	return out
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

		// Additional guardrails:
		//  1) require the rule-based composite to clear probable threshold
		//  2) require either decent fuzzy overlap or strong event signal
		// This prevents same-topic-but-different-question false positives.
		if ruleResult.CompositeScore < m.cfg.ProbableMatchThreshold {
			fmt.Printf("[matcher/search] Rejected by composite gate: llm=%.2f rule=%.2f (<%.2f) | %q vs %q\n",
				llmConf, ruleResult.CompositeScore, m.cfg.ProbableMatchThreshold, lr.MarketA.Title, lr.MarketB.Title)
			continue
		}
		if !ruleResult.SignatureMatch && realFuzzy < 0.50 && ruleResult.EventMatchScore < 0.60 {
			fmt.Printf("[matcher/search] Rejected by semantic gate: fuzzy=%.2f event=%.2f | %q vs %q\n",
				realFuzzy, ruleResult.EventMatchScore, lr.MarketA.Title, lr.MarketB.Title)
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

func addToVenueMap(m *models.CanonicalMarket, polyMap, kalshiMap map[string]*models.CanonicalMarket) {
	switch m.VenueID {
	case models.VenuePolymarket:
		polyMap[m.VenueMarketID] = m
	case models.VenueKalshi:
		kalshiMap[m.VenueMarketID] = m
	}
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
	jaccard := func(a, b string) float64 {
		wa := strings.Fields(strings.ToLower(a))
		wb := strings.Fields(strings.ToLower(b))
		setA := make(map[string]bool, len(wa))
		for _, w := range wa {
			setA[w] = true
		}
		inter, union := 0, len(setA)
		for _, w := range wb {
			if setA[w] {
				inter++
			} else {
				union++
			}
		}
		if union == 0 {
			return 0
		}
		return float64(inter) / float64(union)
	}

	var confirmed []*MatchResult
	for _, poly := range polyMarkets {
		// Pick best kalshi candidate by Jaccard, then run full rule-based compare
		var bestK *models.CanonicalMarket
		bestSim := -1.0
		for _, k := range kalshiMarkets {
			if sim := jaccard(poly.Title, k.Title); sim > bestSim {
				bestSim = sim
				bestK = k
			}
		}
		if bestK == nil {
			continue
		}
		result := m.compare(poly, bestK)
		if result.Confidence == ConfidenceMatch ||
			(result.Confidence == ConfidenceProbable && result.CompositeScore >= m.cfg.MatchThreshold) {
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
			// Keep PROBABLE only when it clears the strict match threshold.
			if r.Confidence == ConfidenceProbable && r.CompositeScore < m.cfg.MatchThreshold {
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

// deduplicateByMarket ensures each individual market ID appears in at most one
// MatchResult. When a market appears in multiple pairs, only the highest-scoring
// pair (first after sorting) is kept.
func deduplicateByMarket(pairs []*MatchResult) []*MatchResult {
	usedA := map[string]bool{}
	usedB := map[string]bool{}
	var out []*MatchResult
	for _, p := range pairs {
		idA := p.MarketA.VenueMarketID
		idB := p.MarketB.VenueMarketID
		if usedA[idA] || usedB[idB] || usedA[idB] || usedB[idA] {
			continue
		}
		usedA[idA] = true
		usedB[idB] = true
		out = append(out, p)
	}
	return out
}

// topPairsByJaccard scores every poly×kalshi combination by Jaccard title
// similarity and returns the top-k unique pairs (each market used at most once).
func topPairsByJaccard(polyMarkets, kalshiMarkets []*models.CanonicalMarket, k int) [][2]*models.CanonicalMarket {
	type pair struct {
		poly   *models.CanonicalMarket
		kalshi *models.CanonicalMarket
		sim    float64
	}

	jaccard := func(a, b string) float64 {
		wa := strings.Fields(strings.ToLower(a))
		wb := strings.Fields(strings.ToLower(b))
		set := make(map[string]bool, len(wa))
		for _, w := range wa {
			set[w] = true
		}
		inter, union := 0, len(set)
		for _, w := range wb {
			if set[w] {
				inter++
			} else {
				union++
			}
		}
		if union == 0 {
			return 0
		}
		return float64(inter) / float64(union)
	}

	var all []pair
	for _, p := range polyMarkets {
		for _, k := range kalshiMarkets {
			all = append(all, pair{p, k, jaccard(p.Title, k.Title)})
		}
	}

	// Sort descending by similarity
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].sim > all[j-1].sim; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}

	// Take top-k, each market used once
	usedPoly := map[string]bool{}
	usedKalshi := map[string]bool{}
	var result [][2]*models.CanonicalMarket
	for _, pr := range all {
		if len(result) >= k {
			break
		}
		if usedPoly[pr.poly.VenueMarketID] || usedKalshi[pr.kalshi.VenueMarketID] {
			continue
		}
		usedPoly[pr.poly.VenueMarketID] = true
		usedKalshi[pr.kalshi.VenueMarketID] = true
		result = append(result, [2]*models.CanonicalMarket{pr.poly, pr.kalshi})
	}
	return result
}

// topByQueryMatch scores markets by how many query words appear in their title
// and returns the top-k. Markets with zero query-word overlap are excluded.
func topByQueryMatch(query string, markets []*models.CanonicalMarket, k int) []*models.CanonicalMarket {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 || len(markets) == 0 {
		if k > len(markets) {
			return markets
		}
		return markets[:k]
	}

	type scored struct {
		m     *models.CanonicalMarket
		score float64
	}
	items := make([]scored, 0, len(markets))
	for _, mm := range markets {
		title := strings.ToLower(mm.Title)
		matched := 0
		for _, w := range words {
			if strings.Contains(title, w) {
				matched++
			}
		}
		if matched > 0 {
			items = append(items, scored{mm, float64(matched) / float64(len(words))})
		}
	}

	// Sort descending by score
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].score > items[j-1].score; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}

	if k > len(items) {
		k = len(items)
	}
	out := make([]*models.CanonicalMarket, k)
	for i := range out {
		out[i] = items[i].m
	}
	return out
}

func countRawPairs(searchResults []SearchResult) int {
	n := 0
	for _, sr := range searchResults {
		n += len(sr.Candidates)
	}
	return n
}

// BatchSearchQueries generates search queries for a set of markets, deduplicating
// markets that would produce the same query.
func BatchSearchQueries(markets []*models.CanonicalMarket) []struct {
	Query   string
	Markets []*models.CanonicalMarket
} {
	type queryGroup struct {
		query   string
		markets []*models.CanonicalMarket
	}
	seen := map[string]*queryGroup{}
	var order []string

	for _, m := range markets {
		q := SearchQueryExtractor(m)
		normalized := strings.ToLower(strings.TrimSpace(q))
		if normalized == "" {
			continue
		}
		if g, ok := seen[normalized]; ok {
			g.markets = append(g.markets, m)
		} else {
			seen[normalized] = &queryGroup{query: q, markets: []*models.CanonicalMarket{m}}
			order = append(order, normalized)
		}
	}

	out := make([]struct {
		Query   string
		Markets []*models.CanonicalMarket
	}, 0, len(order))
	for _, key := range order {
		g := seen[key]
		out = append(out, struct {
			Query   string
			Markets []*models.CanonicalMarket
		}{Query: g.query, Markets: g.markets})
	}
	return out
}

// CrossSearchWorkerPool runs search queries against a target venue in parallel
// with bounded concurrency and rate limiting to avoid 429 errors.
type CrossSearchWorkerPool struct {
	Concurrency int
	// DelayBetweenQueries adds a pause between search queries to respect rate limits.
	DelayBetweenQueries time.Duration
}

// SearchFunc is the signature for a function that searches a venue by query
// and returns normalized canonical markets.
type SearchFunc func(ctx context.Context, query string) ([]*models.CanonicalMarket, error)

// DiversifySourceMarkets selects a diverse set of source markets for cross-search.
// Instead of sending 30 NHL team variants as 30 separate queries, it groups markets
// by their cleaned search query and picks one representative per group.
// Returns at most maxMarkets unique query representatives.
func DiversifySourceMarkets(markets []*models.CanonicalMarket, maxMarkets int) []*models.CanonicalMarket {
	type group struct {
		representative *models.CanonicalMarket
		count          int
	}

	groups := map[string]*group{}
	var order []string

	for _, m := range markets {
		q := strings.ToLower(strings.TrimSpace(SearchQueryExtractor(m)))
		if q == "" {
			continue
		}

		// Further normalize: extract core topic by removing team/player specifics
		// e.g., "the chicago bulls win the 2026 nba finals" and
		//        "the boston celtics win the 2026 nba finals"
		// share the pattern "win the 2026 nba finals"
		coreKey := extractCorePattern(q)

		if g, ok := groups[coreKey]; ok {
			g.count++
			// Keep the one with highest liquidity as representative
			if m.Liquidity > g.representative.Liquidity {
				g.representative = m
			}
		} else {
			groups[coreKey] = &group{representative: m, count: 1}
			order = append(order, coreKey)
		}
	}

	var out []*models.CanonicalMarket
	for _, key := range order {
		if len(out) >= maxMarkets {
			break
		}
		g := groups[key]
		out = append(out, g.representative)
		if g.count > 1 {
			fmt.Printf("[search] Deduplicated %d similar markets into 1 query: %q\n",
				g.count, SearchQueryExtractor(g.representative))
		}
	}
	return out
}

// extractCorePattern finds the common pattern in a market title by removing
// team/entity-specific parts. This groups "X win the 2026 NBA finals" variants
// into one query instead of sending 30 separate team queries.
//
// Only collapses on action verbs (win, qualify, reach, hit), NOT on time
// qualifiers (before, after, by) which are too generic and would incorrectly
// group unrelated markets like "leave trump admin before 2027" with
// "bitcoin hit 100k before 2027".
func extractCorePattern(title string) string {
	patterns := []string{
		" win the ", " qualify for the ", " win ", " reach ", " hit ",
	}
	for _, p := range patterns {
		if idx := strings.Index(title, p); idx >= 0 {
			return strings.TrimSpace(title[idx:])
		}
	}
	return title
}

// RunCrossSearch executes search queries against a target venue, returning
// SearchResults that pair each source market with its candidates.
func (p *CrossSearchWorkerPool) RunCrossSearch(
	ctx context.Context,
	sourceMarkets []*models.CanonicalMarket,
	searchFn SearchFunc,
	maxCandidatesPerQuery int,
) []SearchResult {
	concurrency := p.Concurrency
	if concurrency <= 0 {
		concurrency = 3
	}

	delay := p.DelayBetweenQueries
	if delay == 0 {
		delay = 200 * time.Millisecond // default rate limiting
	}

	type workItem struct {
		source *models.CanonicalMarket
		query  string
	}

	// Build work items, dedup by query
	var work []workItem
	seenQueries := map[string]bool{}
	for _, m := range sourceMarkets {
		q := SearchQueryExtractor(m)
		normalized := strings.ToLower(strings.TrimSpace(q))
		if normalized == "" || seenQueries[normalized] {
			continue
		}
		seenQueries[normalized] = true
		work = append(work, workItem{source: m, query: q})
	}

	fmt.Printf("[search] Running %d cross-search queries (concurrency=%d, delay=%v)...\n",
		len(work), concurrency, delay)

	var mu sync.Mutex
	var results []SearchResult
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	// Rate limiter: one token per delay interval
	ticker := time.NewTicker(delay)
	defer ticker.Stop()

	for _, w := range work {
		// Wait for rate limit token before launching
		<-ticker.C

		wg.Add(1)
		go func(wi workItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			candidates, err := searchFn(ctx, wi.query)
			if err != nil {
				fmt.Printf("[search] WARNING: query %q failed: %v\n", wi.query, err)
				return
			}

			// Limit candidates per query
			if maxCandidatesPerQuery > 0 && len(candidates) > maxCandidatesPerQuery {
				candidates = candidates[:maxCandidatesPerQuery]
			}

			if len(candidates) > 0 {
				mu.Lock()
				results = append(results, SearchResult{
					Source:     wi.source,
					Candidates: candidates,
				})
				mu.Unlock()
			}
		}(w)
	}

	wg.Wait()

	// Deduplicate candidates within each SearchResult
	for i := range results {
		seen := map[string]bool{}
		unique := make([]*models.CanonicalMarket, 0, len(results[i].Candidates))
		for _, c := range results[i].Candidates {
			if !seen[c.VenueMarketID] {
				seen[c.VenueMarketID] = true
				unique = append(unique, c)
			}
		}
		results[i].Candidates = unique
	}

	totalCandidates := 0
	for _, r := range results {
		totalCandidates += len(r.Candidates)
	}
	fmt.Printf("[search] Cross-search complete: %d queries returned %d total candidates\n",
		len(results), totalCandidates)
	return results
}
