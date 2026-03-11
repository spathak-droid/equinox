package matcher

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/equinox/internal/models"
)

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
