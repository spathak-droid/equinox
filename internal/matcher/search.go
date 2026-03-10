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

// SearchQueryExtractor builds a search query from a CanonicalMarket title.
// It strips prediction-market boilerplate to produce better search queries.
func SearchQueryExtractor(m *models.CanonicalMarket) string {
	return cleanTitleForSearch(m.Title)
}

// cleanTitleForSearch removes common prediction-market phrasing that adds noise
// to search queries without adding semantic value.
func cleanTitleForSearch(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))

	// Remove common question prefixes
	prefixes := []string{
		"will ",
		"will the ",
		"will a ",
		"is ",
		"are ",
		"does ",
		"do ",
		"can ",
		"should ",
		"has ",
		"have ",
		"what is the probability that ",
		"what are the chances that ",
		"what is the chance that ",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(t, p) {
			t = t[len(p):]
			break
		}
	}

	// Remove trailing question marks and common suffixes
	t = strings.TrimRight(t, "?")
	suffixes := []string{
		" by end of year",
		" by year end",
		" before end of year",
		" in 2024",
		" in 2025",
		" in 2026",
		" this year",
		" this month",
	}
	for _, s := range suffixes {
		t = strings.TrimSuffix(t, s)
	}

	t = strings.TrimSpace(t)

	// If the cleaned query is too short, use the original title
	if len(t) < 5 {
		return strings.TrimSpace(title)
	}

	// Capitalize first letter back for readability (search APIs may use it)
	if len(t) > 0 {
		t = strings.ToUpper(t[:1]) + t[1:]
	}

	return t
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

// FindEquivalentPairsFromSearch scores pre-found search candidates using the
// same 4-stage pipeline as brute-force, but only on the small candidate set
// produced by cross-venue search.
//
// It deduplicates candidates before scoring: if 10 source markets all found the
// same 2 candidates via search, only 2 unique pairs are scored (not 20).
func (m *Matcher) FindEquivalentPairsFromSearch(ctx context.Context, searchResults []SearchResult) []*MatchResult {
	// Deduplicate before scoring — this is where the 18/20 duplicate problem is fixed
	deduped := deduplicateSearchResults(searchResults)

	totalPairs := 0
	for _, sr := range deduped {
		totalPairs += len(sr.Candidates)
	}
	fmt.Printf("[matcher/search] Scoring %d unique search-candidate pairs (deduplicated from %d raw)...\n",
		totalPairs, countRawPairs(searchResults))

	var confirmed []*MatchResult
	var ambiguous []*MatchResult

	for _, sr := range deduped {
		for _, candidate := range sr.Candidates {
			result := m.compare(sr.Source, candidate)
			switch result.Confidence {
			case ConfidenceMatch:
				confirmed = append(confirmed, result)
			case ConfidenceProbable:
				ambiguous = append(ambiguous, result)
			}
		}
	}

	fmt.Printf("[matcher/search] Stages 1-3 complete: %d confirmed, %d ambiguous, %d rejected\n",
		len(confirmed), len(ambiguous), totalPairs-len(confirmed)-len(ambiguous))

	// Stage 4: LLM disambiguation for ambiguous pairs
	if m.openai != nil && len(ambiguous) > 0 {
		resolved := m.disambiguateWithLLM(ctx, ambiguous)
		for _, r := range resolved {
			if r.Confidence != ConfidenceNoMatch {
				confirmed = append(confirmed, r)
			}
		}
	} else if len(ambiguous) > 0 {
		fmt.Printf("[matcher/search] No LLM available — filtering %d ambiguous pairs (keeping composite >= %.2f)\n",
			len(ambiguous), m.cfg.MatchThreshold)
		for _, r := range ambiguous {
			if r.CompositeScore >= m.cfg.MatchThreshold {
				confirmed = append(confirmed, r)
			}
		}
	}

	// Final dedup (same pair found from both search directions A→B and B→A)
	confirmed = DeduplicatePairs(confirmed)

	// Sort by composite score descending
	for i := 1; i < len(confirmed); i++ {
		for j := i; j > 0 && confirmed[j].CompositeScore > confirmed[j-1].CompositeScore; j-- {
			confirmed[j], confirmed[j-1] = confirmed[j-1], confirmed[j]
		}
	}

	for _, r := range confirmed {
		fmt.Printf("[matcher/search] %s — %s vs %s (score=%.3f): %s\n",
			r.Confidence, r.MarketA.VenueID, r.MarketB.VenueID, r.CompositeScore, r.Explanation)
	}

	return confirmed
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
