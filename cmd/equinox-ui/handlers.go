package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/news"
	"github.com/equinox/internal/router"
	"github.com/equinox/internal/storage"
	"github.com/equinox/internal/venues/kalshi"
)

// parseLimitParam reads the "limit" query parameter from the request.
// It returns defaultVal unless the parameter is a valid integer in (0, maxVal].
func parseLimitParam(r *http.Request, defaultVal, maxVal int) int {
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, e := strconv.Atoi(l); e == nil && n > 0 && n <= maxVal {
			return n
		}
	}
	return defaultVal
}

// truncateQuery truncates a query string to maxLen characters.
func truncateQuery(q string, maxLen int) string {
	if len(q) > maxLen {
		return q[:maxLen]
	}
	return q
}

// handleAPIPairs returns matched pairs from the index as JSON.
func handleAPIPairs(cfg *config.Config, storePtr *atomic.Pointer[storage.Store]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store := storePtr.Load()
		if store == nil {
			http.Error(w, "index not available", http.StatusServiceUnavailable)
			return
		}
		query := truncateQuery(strings.TrimSpace(r.URL.Query().Get("q")), 500)
		limit := parseLimitParam(r, 50, 200)

		data, err := runIndexedBrowse(cfg, store, query, limit)
		if err != nil {
			log.Printf("handleAPIPairs: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		json.NewEncoder(w).Encode(map[string]any{
			"query":          query,
			"pairs":          data.Pairs,
			"match_count":    data.MatchCount,
			"probable_count": data.ProbableCount,
			"index_stats":    data.IndexStats,
		})
	}
}

// handleAPIStats returns index statistics as JSON.
func handleAPIStats(storePtr *atomic.Pointer[storage.Store]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store := storePtr.Load()
		if store == nil {
			http.Error(w, "index not available", http.StatusServiceUnavailable)
			return
		}
		stats, err := store.GetStats()
		if err != nil {
			log.Printf("handleAPIStats: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	}
}

// handleNews returns news articles for a query as JSON.
func handleNews(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := truncateQuery(strings.TrimSpace(r.URL.Query().Get("q")), 500)
		if query == "" {
			http.Error(w, "missing q", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		fetcher := news.NewFetcher(cfg.HTTPTimeout, cfg.NewsMaxArticles)
		mn := fetcher.FetchForQuery(ctx, query)
		var articles []NewsArticleView
		if mn != nil {
			articles = toNewsArticleViews(mn)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		json.NewEncoder(w).Encode(map[string]any{
			"query":    query,
			"articles": articles,
		})
	}
}

// runIndexedBrowse runs matching against the SQLite index and returns pairs.
// If query is non-empty, it FTS-searches for relevant markets first.
func runIndexedBrowse(cfg *config.Config, store *storage.Store, query string, limit int) (*PageData, error) {
	mtch := matcher.New(cfg)
	r := router.New(cfg)

	var searchResults []matcher.SearchResult

	if query != "" {
		// Search mode: FTS search for the query, get top results from each venue.
		// Try AND-based search first (all words must match), fall back to OR if empty.
		polyResults, _ := store.SearchByTitle(query, string(models.VenueKalshi), 20)
		kalshiResults, _ := store.SearchByTitle(query, string(models.VenuePolymarket), 20)
		if len(polyResults) == 0 {
			polyResults, _ = store.SearchByTitleOR(query, string(models.VenueKalshi), 20)
		}
		if len(kalshiResults) == 0 {
			kalshiResults, _ = store.SearchByTitleOR(query, string(models.VenuePolymarket), 20)
		}

		// Cross-match: use the original query (not full titles) to find
		// candidates from the other venue. This avoids 80+ expensive FTS queries.
		// Each poly market is paired with all kalshi results as candidates, and vice versa.
		for _, pm := range polyResults {
			if len(kalshiResults) > 0 {
				searchResults = append(searchResults, matcher.SearchResult{
					Source:     pm,
					Candidates: kalshiResults,
				})
			}
		}
	} else {
		// Browse all: check cache first
		browseCache.Lock()
		if browseCache.data != nil && time.Now().Before(browseCache.expiresAt) {
			cached := browseCache.data
			browseCache.Unlock()
			return cached, nil
		}
		browseCache.Unlock()

		// Browse all: load top markets from each venue and cross-search
		polyMarkets, err := store.GetTopMarketsLite(string(models.VenuePolymarket), 50)
		if err != nil {
			return nil, fmt.Errorf("loading polymarket: %w", err)
		}
		for _, pm := range polyMarkets {
			candidates, err := store.SearchByTitleOR(pm.Title, string(models.VenuePolymarket), 10)
			if err != nil || len(candidates) == 0 {
				continue
			}
			searchResults = append(searchResults, matcher.SearchResult{
				Source:     pm,
				Candidates: candidates,
			})
		}
	}

	pairs := mtch.FindMatchesRelaxed(searchResults, 10, 0.20)
	if len(pairs) > limit {
		pairs = pairs[:limit]
	}

	// Build pair views
	var pairViews []PairView
	matchCount, probableCount := 0, 0
	for _, p := range pairs {
		pv := matchToPairView(cfg, r, p)
		pairViews = append(pairViews, pv)
		switch pv.Confidence {
		case string(matcher.ConfidenceMatch):
			matchCount++
		case string(matcher.ConfidenceProbable):
			probableCount++
		}
	}

	stats, _ := store.GetStats()

	result := &PageData{
		SearchQuery: query,
		Pairs:       pairViews,
		VenueCounts: map[models.VenueID]int{
			models.VenuePolymarket: stats.ByVenue[string(models.VenuePolymarket)],
			models.VenueKalshi:     stats.ByVenue[string(models.VenueKalshi)],
		},
		MatchCount:    matchCount,
		ProbableCount: probableCount,
		HasQuery:      true,
		BrowseMode:    true,
		IndexStats: &IndexStats{
			Total:      stats.Total,
			Polymarket: stats.ByVenue[string(models.VenuePolymarket)],
			Kalshi:     stats.ByVenue[string(models.VenueKalshi)],
			LastUpdate: stats.LastUpdate,
		},
	}

	// Cache browse-all results for 5 minutes
	if query == "" {
		browseCache.Lock()
		browseCache.data = result
		browseCache.expiresAt = time.Now().Add(5 * time.Minute)
		browseCache.Unlock()
	}

	return result, nil
}

// runQdrantSearch embeds the query, searches Qdrant for similar markets,
// then hydrates full market data from SQLite and runs cross-venue matching.
func runQdrantSearch(ctx context.Context, cfg *config.Config, kalshiClient *kalshi.Client, qdrant *storage.QdrantClient, embedder *storage.EmbeddingClient, store *storage.Store, query string, topK int) (*PageData, error) {
	// Embed the search query
	queryVec, err := embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	// Search Qdrant separately per venue to guarantee cross-venue results.
	// Filter to only markets with real titles (skip combo/parlay garbage).
	venueFilter := func(venue string) map[string]any {
		return map[string]any{
			"must": []map[string]any{
				{"key": "venue_id", "match": map[string]any{"value": venue}},
			},
		}
	}

	polyResults, err := qdrant.Search(ctx, queryVec, topK, venueFilter(string(models.VenuePolymarket)))
	if err != nil {
		return nil, fmt.Errorf("qdrant search polymarket: %w", err)
	}
	kalshiResults, err := qdrant.Search(ctx, queryVec, topK, venueFilter(string(models.VenueKalshi)))
	if err != nil {
		return nil, fmt.Errorf("qdrant search kalshi: %w", err)
	}

	if len(polyResults) == 0 && len(kalshiResults) == 0 {
		stats, _ := store.GetStats()
		return &PageData{
			SearchQuery: query,
			HasQuery:    true,
			VenueCounts: map[models.VenueID]int{},
			IndexStats: &IndexStats{
				Total:      stats.Total,
				Polymarket: stats.ByVenue[string(models.VenuePolymarket)],
				Kalshi:     stats.ByVenue[string(models.VenueKalshi)],
				LastUpdate: stats.LastUpdate,
			},
		}, nil
	}

	// Hydrate full market data from SQLite, skipping garbage titles and bracket markets.
	// Also record Qdrant similarity scores per market for display.
	qdrantScores := map[string]float64{} // key: "venueID:marketID" -> cosine similarity
	hydrate := func(results []storage.QdrantSearchResult, label string) []*models.CanonicalMarket {
		var markets []*models.CanonicalMarket
		for _, r := range results {
			venueID, _ := r.Payload["venue_id"].(string)
			marketID, _ := r.Payload["venue_market_id"].(string)
			if venueID == "" || marketID == "" {
				fmt.Printf("[equinox-ui] hydrate(%s): skip empty id (payload=%v)\n", label, r.Payload)
				continue
			}
			m, err := store.GetMarket(venueID, marketID)
			if err != nil || m == nil {
				fmt.Printf("[equinox-ui] hydrate(%s): skip %s/%s (not in SQLite, err=%v)\n", label, venueID, marketID, err)
				continue
			}
			if isGarbageMarket(m) {
				fmt.Printf("[equinox-ui] hydrate(%s): skip garbage %q\n", label, m.Title)
				continue
			}
			qdrantScores[venueID+":"+marketID] = r.Score
			markets = append(markets, m)
		}
		return markets
	}

	polyMarkets := hydrate(polyResults, "poly")
	kalshiMarkets := hydrate(kalshiResults, "kalshi")

	// If Qdrant results couldn't be hydrated from SQLite (stale vector index),
	// fall back to FTS search which works directly from SQLite.
	if len(polyMarkets) == 0 && len(kalshiMarkets) == 0 {
		fmt.Printf("[equinox-ui] Qdrant hydration returned 0 markets, falling back to FTS\n")
		return nil, fmt.Errorf("qdrant results not in SQLite (stale vector index)")
	}

	// Fetch images for Kalshi markets missing them (v2 indexed data has no images)
	var needImages []string
	for _, m := range kalshiMarkets {
		if m.ImageURL == "" && m.VenueEventTicker != "" {
			needImages = append(needImages, m.VenueEventTicker)
		}
	}
	if len(needImages) > 0 {
		imgMap := kalshiClient.FetchEventImages(ctx, needImages)
		for _, m := range kalshiMarkets {
			if m.ImageURL == "" {
				if img, ok := imgMap[m.VenueEventTicker]; ok {
					m.ImageURL = img
				}
			}
		}
	}

	// Supplement Qdrant results with FTS cross-search for better coverage.
	// Qdrant may not have vectors for all markets (embedding failures, etc.).
	// Limit to top 10 markets per venue to keep response fast.
	kalshiSeen := map[string]bool{}
	for _, m := range kalshiMarkets {
		kalshiSeen[m.VenueMarketID] = true
	}
	// Also do a single FTS OR search with the original query to get Kalshi matches directly
	ftsKalshi, _ := store.SearchByTitleOR(query, string(models.VenuePolymarket), 20)
	for _, fm := range ftsKalshi {
		if fm.VenueID != models.VenueKalshi || kalshiSeen[fm.VenueMarketID] {
			continue
		}
		if isGarbageMarket(fm) {
			continue
		}
		kalshiMarkets = append(kalshiMarkets, fm)
		kalshiSeen[fm.VenueMarketID] = true
	}
	// FTS cross-search: only top 10 poly markets -> kalshi
	ftsLimit := 10
	if len(polyMarkets) < ftsLimit {
		ftsLimit = len(polyMarkets)
	}
	for _, pm := range polyMarkets[:ftsLimit] {
		ftsResults, _ := store.SearchByTitle(pm.Title, string(models.VenuePolymarket), 5)
		for _, fm := range ftsResults {
			if fm.VenueID != models.VenueKalshi || kalshiSeen[fm.VenueMarketID] {
				continue
			}
			if isGarbageMarket(fm) {
				continue
			}
			kalshiMarkets = append(kalshiMarkets, fm)
			kalshiSeen[fm.VenueMarketID] = true
		}
	}

	// Build cross-venue candidate pairs: for each market, find its best match from the other venue.
	// Uses entity-weighted scoring so "Miami Heat" pairs with "Miami", not "Dallas".
	ranked := matcher.RankCandidatesByBestMatch(polyMarkets, kalshiMarkets, 20)

	fmt.Printf("[equinox-ui] Qdrant: %d poly + %d kalshi -> %d candidates for LLM verification\n",
		len(polyMarkets), len(kalshiMarkets), len(ranked))
	for i, c := range ranked {
		if i < 5 {
			fmt.Printf("[equinox-ui]   pair[%d]: %q vs %q\n", i, c.Source.Title, c.Candidate.Title)
		}
	}

	rtr := router.New(cfg)
	var pairViews []PairView
	matchCount := 0
	pairedIDs := map[string]bool{}

	if len(ranked) > 0 && cfg.OpenAIAPIKey != "" {
		verified, err := matcher.VerifyPairsWithLLM(ctx, cfg, ranked)
		if err != nil {
			fmt.Printf("[equinox-ui] LLM verification failed: %v\n", err)
		} else {
			for _, vp := range verified {
				// Look up Qdrant cosine similarity for each market in the pair
				scoreA := qdrantScores[string(vp.MarketA.VenueID)+":"+vp.MarketA.VenueMarketID]
				scoreB := qdrantScores[string(vp.MarketB.VenueID)+":"+vp.MarketB.VenueMarketID]
				embScore := (scoreA + scoreB) / 2.0
				// Wrap in MatchResult for the view pipeline
				mr := &matcher.MatchResult{
					MarketA:        vp.MarketA,
					MarketB:        vp.MarketB,
					Confidence:     matcher.ConfidenceMatch,
					CompositeScore: 1.0,
					EmbeddingScore: embScore,
					Explanation:    "LLM verified: " + vp.Reason,
				}
				pv := matchToPairView(cfg, rtr, mr)
				pairViews = append(pairViews, pv)
				pairedIDs[string(vp.MarketA.VenueID)+":"+vp.MarketA.VenueMarketID] = true
				pairedIDs[string(vp.MarketB.VenueID)+":"+vp.MarketB.VenueMarketID] = true
				matchCount++
			}
		}
	}

	// Sort pairs by embedding score descending (most relevant to query first)
	sort.Slice(pairViews, func(i, j int) bool {
		return pairViews[i].EmbeddingScore > pairViews[j].EmbeddingScore
	})

	// Collect unpaired markets, interleaving venues for better display.
	// Markets with actual pricing appear before zero-price markets.
	var unpairedWithPrice, unpairedZeroPrice []MarketView
	allMarkets := append(polyMarkets, kalshiMarkets...)
	for _, m := range allMarkets {
		key := string(m.VenueID) + ":" + m.VenueMarketID
		if !pairedIDs[key] {
			mv := toMarketView(m)
			if m.YesPrice > 0 || m.NoPrice > 0 {
				unpairedWithPrice = append(unpairedWithPrice, mv)
			} else {
				unpairedZeroPrice = append(unpairedZeroPrice, mv)
			}
		}
	}
	unpaired := append(unpairedWithPrice, unpairedZeroPrice...)

	stats, _ := store.GetStats()
	return &PageData{
		SearchQuery:     query,
		Pairs:           pairViews,
		UnpairedMarkets: unpaired,
		VenueCounts: map[models.VenueID]int{
			models.VenuePolymarket: len(polyMarkets),
			models.VenueKalshi:     len(kalshiMarkets),
		},
		MatchCount:    matchCount,
		ProbableCount: 0,
		HasQuery:      true,
		IndexStats: &IndexStats{
			Total:      stats.Total,
			Polymarket: stats.ByVenue[string(models.VenuePolymarket)],
			Kalshi:     stats.ByVenue[string(models.VenueKalshi)],
			LastUpdate: stats.LastUpdate,
		},
	}, nil
}
