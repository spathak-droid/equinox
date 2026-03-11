package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/normalizer"
	"github.com/equinox/internal/router"
	"github.com/equinox/internal/venues"
	"github.com/equinox/internal/venues/kalshi"
	"github.com/equinox/internal/venues/polymarket"
)

const maxPages = 3
const maxPagesDeep = 8

// runSearchPipelineWithProgress fetches paged search results from both venues
// and returns the best cross-venue pairs found.
// In default mode it stops after the first non-empty pair set.
// In deep-search mode it keeps paginating to grow the market pool.
func runSearchPipelineWithProgress(cfg *config.Config, kalshiClient *kalshi.Client, polyClient *polymarket.Client, query string, deepSearch bool, emit func(progressEvent)) (*PageData, error) {
	if emit == nil {
		emit = func(progressEvent) {}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	norm := normalizer.New(cfg)
	m := matcher.New(cfg)
	r := router.New(cfg)

	var allPolyMarkets, allKalshiMarkets []*models.CanonicalMarket
	seenPoly := map[string]bool{}
	seenKalshi := map[string]bool{}
	// Track which market pairs have already been emitted to avoid duplicates.
	emittedPairs := map[string]bool{}
	pairIndex := 0

	polyOffset := 0
	kalshiCursor := ""
	polyDone := false
	kalshiDone := false
	page := 0
	stagnationPages := 0
	var pairs []*matcher.MatchResult

	pageLimit := maxPages
	if deepSearch {
		pageLimit = maxPagesDeep
	}
	// Default mode stops after first non-empty result set; deep mode keeps
	// paginating to expand the market pool and potentially find better matches.
	for (deepSearch || len(pairs) == 0) && !(polyDone && kalshiDone) && page < pageLimit {
		page++
		emit(progressEvent{Type: "step", Msg: fmt.Sprintf("Fetching page %d for \"%s\"...", page, query)})

		var wg sync.WaitGroup
		var polyRaw, kalshiRaw []*venues.RawMarket
		var nextPolyOffset int
		var nextKalshiCursor string

		if !polyDone {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var err error
				polyRaw, nextPolyOffset, err = polyClient.FetchMarketsByQueryPaged(ctx, query, polyOffset)
				if err != nil {
					fmt.Printf("[equinox-ui] WARNING: polymarket page %d: %v\n", page, err)
				}
				if nextPolyOffset == 0 {
					polyDone = true
				}
			}()
		}
		if !kalshiDone {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var err error
				kalshiRaw, nextKalshiCursor, err = kalshiClient.FetchMarketsByQueryPaged(ctx, query, kalshiCursor, 100)
				if err != nil {
					fmt.Printf("[equinox-ui] WARNING: kalshi page %d: %v\n", page, err)
				}
				if nextKalshiCursor == "" {
					kalshiDone = true
				}
			}()
		}
		wg.Wait()

		polyOffset = nextPolyOffset
		kalshiCursor = nextKalshiCursor

		// Normalize and deduplicate new markets
		newPoly, _ := norm.Normalize(ctx, polyRaw)
		newKalshi, _ := norm.Normalize(ctx, kalshiRaw)
		addedPoly := 0
		addedKalshi := 0
		for _, mm := range newPoly {
			if !seenPoly[mm.VenueMarketID] {
				seenPoly[mm.VenueMarketID] = true
				allPolyMarkets = append(allPolyMarkets, mm)
				addedPoly++
			}
		}
		for _, mm := range newKalshi {
			if !seenKalshi[mm.VenueMarketID] {
				seenKalshi[mm.VenueMarketID] = true
				allKalshiMarkets = append(allKalshiMarkets, mm)
				addedKalshi++
			}
		}

		emit(progressEvent{Type: "result",
			Msg:   fmt.Sprintf("Pool: %d poly, %d kalshi markets", len(allPolyMarkets), len(allKalshiMarkets)),
			Count: len(allPolyMarkets) + len(allKalshiMarkets),
		})

		// Cross-pollinate on accumulated pool
		pairs = m.CrossPollinateJaccard(allPolyMarkets, allKalshiMarkets)
		fmt.Printf("[equinox-ui] Page %d: pool poly=%d kalshi=%d → %d pairs\n",
			page, len(allPolyMarkets), len(allKalshiMarkets), len(pairs))

		// Stream any newly discovered pairs
		for _, p := range pairs {
			pairKey := p.MarketA.VenueMarketID + "|" + p.MarketB.VenueMarketID
			if emittedPairs[pairKey] {
				continue
			}
			emittedPairs[pairKey] = true
			pairIndex++
			if pairIndex <= maxDisplayPairs {
				pv := matchToPairView(cfg, r, p)
				emit(progressEvent{Type: "pair", Pair: &pv, Index: pairIndex})
			}
		}

		// Deep mode can hit venue-side pagination loops that return the same set.
		// Stop after repeated no-growth pages and switch to query-variant expansion.
		if deepSearch {
			if addedPoly == 0 && addedKalshi == 0 {
				stagnationPages++
			} else {
				stagnationPages = 0
			}
			if stagnationPages >= 2 {
				emit(progressEvent{Type: "step", Msg: "No new markets on additional pages; widening search terms..."})
				break
			}
		}
	}

	// In deep mode, broaden search with query variants to recover matches
	// that don't appear on the top ranked page for the literal query.
	if deepSearch {
		for _, variant := range expandQueryVariants(query) {
			emit(progressEvent{Type: "step", Msg: fmt.Sprintf("Trying related search: %q", variant)})

			var wg sync.WaitGroup
			var polyRaw, kalshiRaw []*venues.RawMarket

			wg.Add(2)
			go func() {
				defer wg.Done()
				var err error
				polyRaw, _, err = polyClient.FetchMarketsByQueryPaged(ctx, variant, 0)
				if err != nil {
					fmt.Printf("[equinox-ui] WARNING: polymarket variant %q: %v\n", variant, err)
				}
			}()
			go func() {
				defer wg.Done()
				var err error
				kalshiRaw, _, err = kalshiClient.FetchMarketsByQueryPaged(ctx, variant, "", 100)
				if err != nil {
					fmt.Printf("[equinox-ui] WARNING: kalshi variant %q: %v\n", variant, err)
				}
			}()
			wg.Wait()

			newPoly, _ := norm.Normalize(ctx, polyRaw)
			newKalshi, _ := norm.Normalize(ctx, kalshiRaw)
			addedPoly := mergeUniqueMarkets(&allPolyMarkets, seenPoly, newPoly)
			addedKalshi := mergeUniqueMarkets(&allKalshiMarkets, seenKalshi, newKalshi)
			if addedPoly == 0 && addedKalshi == 0 {
				continue
			}

			pairs = m.CrossPollinateJaccard(allPolyMarkets, allKalshiMarkets)
			// Stream newly discovered pairs from variant expansion
			for _, p := range pairs {
				pairKey := p.MarketA.VenueMarketID + "|" + p.MarketB.VenueMarketID
				if emittedPairs[pairKey] {
					continue
				}
				emittedPairs[pairKey] = true
				pairIndex++
				if pairIndex <= maxDisplayPairs {
					pv := matchToPairView(cfg, r, p)
					emit(progressEvent{Type: "pair", Pair: &pv, Index: pairIndex})
				}
			}
			emit(progressEvent{
				Type:  "result",
				Msg:   fmt.Sprintf("Expanded pool via %q: +%d poly, +%d kalshi", variant, addedPoly, addedKalshi),
				Count: len(allPolyMarkets) + len(allKalshiMarkets),
			})
		}
	}

	logRankedCrossMatches(query, allPolyMarkets, allKalshiMarkets)

	if len(pairs) == 0 {
		emit(progressEvent{Type: "result", Msg: "No equivalent pairs found"})
	} else {
		emit(progressEvent{Type: "result", Msg: fmt.Sprintf("Found %d equivalent pair(s)", len(pairs)), Count: len(pairs)})
	}

	polyMarkets := allPolyMarkets
	kalshiMarkets := allKalshiMarkets

	venueCounts := map[models.VenueID]int{
		models.VenuePolymarket: len(polyMarkets),
		models.VenueKalshi:     len(kalshiMarkets),
	}

	var allMarkets []*models.CanonicalMarket
	allMarkets = append(allMarkets, polyMarkets...)
	allMarkets = append(allMarkets, kalshiMarkets...)

	data, err := buildPageData(cfg, ctx, m, allMarkets, pairs, venueCounts, query)
	if err != nil {
		return nil, err
	}
	data.DeepSearch = deepSearch
	return data, nil
}

func mergeUniqueMarkets(dst *[]*models.CanonicalMarket, seen map[string]bool, incoming []*models.CanonicalMarket) int {
	added := 0
	for _, m := range incoming {
		if seen[m.VenueMarketID] {
			continue
		}
		seen[m.VenueMarketID] = true
		*dst = append(*dst, m)
		added++
	}
	return added
}

// expandQueryVariants builds a small set of high-signal alternatives for deep search.
func expandQueryVariants(query string) []string {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}

	stop := map[string]bool{
		"will": true, "the": true, "a": true, "an": true, "in": true, "on": true,
		"for": true, "of": true, "to": true, "and": true, "or": true, "is": true,
	}

	words := strings.Fields(q)
	var keep []string
	var years []string
	for _, w := range words {
		wl := strings.ToLower(strings.TrimSpace(w))
		if wl == "" {
			continue
		}
		if len(wl) == 4 && wl[0] >= '0' && wl[0] <= '9' && wl[1] >= '0' && wl[1] <= '9' && wl[2] >= '0' && wl[2] <= '9' && wl[3] >= '0' && wl[3] <= '9' {
			years = append(years, wl)
			continue
		}
		if stop[wl] {
			continue
		}
		keep = append(keep, w)
	}

	seen := map[string]bool{strings.ToLower(q): true}
	var variants []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		k := strings.ToLower(s)
		if seen[k] {
			return
		}
		seen[k] = true
		variants = append(variants, s)
	}

	if len(keep) >= 2 {
		add(strings.Join(keep, " "))
	}
	if len(years) > 0 && len(keep) > 0 {
		add(years[0] + " " + strings.Join(keep, " "))
		add(strings.Join(keep, " ") + " " + years[0])
	}
	if len(keep) >= 3 {
		add(strings.Join(keep[:3], " "))
	}
	return variants
}

// logQueryMatchScores logs, for each market, what fraction of the query words
// appear in the market title. Helps diagnose why a search returned certain results.
func logQueryMatchScores(query string, venues ...[]*models.CanonicalMarket) {
	queryWords := strings.Fields(strings.ToLower(query))
	if len(queryWords) == 0 {
		return
	}
	fmt.Printf("[query-match] query=%q words=%v\n", query, queryWords)
	for _, markets := range venues {
		for _, m := range markets {
			title := strings.ToLower(m.Title)
			matched := 0
			for _, w := range queryWords {
				if strings.Contains(title, w) {
					matched++
				}
			}
			pct := float64(matched) / float64(len(queryWords)) * 100
			fmt.Printf("[query-match] %5.0f%% (%d/%d) [%s] %q\n",
				pct, matched, len(queryWords), m.VenueID, m.Title)
		}
	}
}

// logCrossVenueWordOverlap logs, for every Poly×Kalshi pair, the fraction of
// Poly title words that appear in the Kalshi title. Helps spot matches that
// share words but are phrased very differently.
func logCrossVenueWordOverlap(polyMarkets, kalshiMarkets []*models.CanonicalMarket) {
	fmt.Printf("[cross-overlap] comparing %d poly × %d kalshi markets\n", len(polyMarkets), len(kalshiMarkets))
	for _, p := range polyMarkets {
		polyWords := strings.Fields(strings.ToLower(p.Title))
		if len(polyWords) == 0 {
			continue
		}
		for _, k := range kalshiMarkets {
			kalshiTitle := strings.ToLower(k.Title)
			matched := 0
			for _, w := range polyWords {
				if strings.Contains(kalshiTitle, w) {
					matched++
				}
			}
			pct := float64(matched) / float64(len(polyWords)) * 100
			if pct >= 50 { // only log pairs with at least 50% overlap
				fmt.Printf("[cross-overlap] %5.0f%% (%d/%d) poly=%q | kalshi=%q\n",
					pct, matched, len(polyWords), p.Title, k.Title)
			}
		}
	}
}

// logRankedCrossMatches takes the top-10 poly markets by query-match score,
// then for each finds its single best kalshi match by Jaccard title similarity.
// Results are ranked by that similarity score.
func logRankedCrossMatches(query string, polyMarkets, kalshiMarkets []*models.CanonicalMarket) {
	queryWords := strings.Fields(strings.ToLower(query))
	if len(queryWords) == 0 || len(polyMarkets) == 0 || len(kalshiMarkets) == 0 {
		return
	}

	queryScore := func(m *models.CanonicalMarket) float64 {
		title := strings.ToLower(m.Title)
		matched := 0
		for _, w := range queryWords {
			if strings.Contains(title, w) {
				matched++
			}
		}
		return float64(matched) / float64(len(queryWords))
	}

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

	// Rank poly markets by query score, take top 10
	type polyScored struct {
		m      *models.CanonicalMarket
		qscore float64
	}
	polyItems := make([]polyScored, len(polyMarkets))
	for i, m := range polyMarkets {
		polyItems[i] = polyScored{m, queryScore(m)}
	}
	for i := 1; i < len(polyItems); i++ {
		for j := i; j > 0 && polyItems[j].qscore > polyItems[j-1].qscore; j-- {
			polyItems[j], polyItems[j-1] = polyItems[j-1], polyItems[j]
		}
	}
	if len(polyItems) > 10 {
		polyItems = polyItems[:10]
	}

	// For each top-10 poly, find the single best kalshi match by Jaccard
	type pair struct {
		poly       *models.CanonicalMarket
		kalshi     *models.CanonicalMarket
		polyQ      float64
		kalshiQ    float64
		similarity float64
	}
	var pairs []pair
	for _, p := range polyItems {
		var bestK *models.CanonicalMarket
		bestSim := -1.0
		bestKQ := 0.0
		for _, k := range kalshiMarkets {
			sim := jaccard(p.m.Title, k.Title)
			if sim > bestSim {
				bestSim = sim
				bestK = k
				bestKQ = queryScore(k)
			}
		}
		if bestK != nil {
			pairs = append(pairs, pair{p.m, bestK, p.qscore, bestKQ, bestSim})
		}
	}

	// Rank pairs by similarity descending
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && pairs[j].similarity > pairs[j-1].similarity; j-- {
			pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
		}
	}

	fmt.Printf("[ranked-cross] top-10 poly by query-match, each paired with best kalshi by title similarity\n")
	for i, pr := range pairs {
		fmt.Printf("[ranked-cross] #%02d sim=%.2f (poly=%.0f%% kalshi=%.0f%%)\n  poly:  %q\n  kalshi:%q\n",
			i+1, pr.similarity, pr.polyQ*100, pr.kalshiQ*100, pr.poly.Title, pr.kalshi.Title)
	}
}
