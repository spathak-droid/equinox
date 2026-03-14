// Command equinox is the entry point for the Equinox cross-venue routing prototype.
//
// Usage:
//
//	go run ./cmd/equinox
//
// The system will:
//  1. Fetch top N markets from Polymarket and Kalshi
//  2. For each Polymarket market, search Kalshi for candidates (and vice versa)
//  3. Score and match the candidates
//  4. Simulate routing decisions for each matched pair
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/news"
	"github.com/equinox/internal/normalizer"
	"github.com/equinox/internal/router"
	"github.com/equinox/internal/storage"
	"github.com/equinox/internal/venues/kalshi"
	"github.com/equinox/internal/venues/polymarket"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	if err := run(); err != nil {
		log.Fatalf("equinox: %v", err)
	}
}

func run() error {
	mode := flag.String("mode", "route", "run mode: route, match, or llm-eval")
	side := flag.String("side", string(router.SideYes), "order side: YES or NO")
	maxPairs := flag.Int("max-pairs", 10, "maximum pairs to output")
	numMarkets := flag.Int("n", 10, "number of markets to fetch from each venue")
	output := flag.String("output", "text", "output format: text or json")
	newsFlag := flag.Bool("news", false, "fetch related news articles for matched pairs")
	indexed := flag.Bool("indexed", false, "use SQLite index for matching (run cmd/indexer first)")
	dbPath := flag.String("db", "equinox_markets.db", "path to SQLite database (with -indexed)")
	topN := flag.Int("top-n", 5, "candidates per market from FTS search (with -indexed)")
	flag.Parse()

	orderSide := router.SideYes
	if strings.ToUpper(*side) == string(router.SideNo) || strings.ToUpper(*side) == "N" {
		orderSide = router.SideNo
	}
	jsonMode := strings.ToLower(*output) == "json"
	runRouting := strings.ToLower(*mode) == "route"
	logf := func(format string, args ...any) {
		if !jsonMode {
			fmt.Printf(format, args...)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mtch := matcher.New(cfg)

	var pairs []*matcher.MatchResult

	if *indexed {
		// ── Indexed mode: use SQLite database for matching ───────────────
		pairs, err = runIndexedMatching(ctx, cfg, mtch, *dbPath, *topN, logf)
		if err != nil {
			return err
		}
	} else {
		// ── Live mode: fetch from APIs and cross-search ─────────────────
		pairs, err = runLiveMatching(ctx, cfg, mtch, *numMarkets, logf)
		if err != nil {
			return err
		}
	}
	logf("\n[main] Found %d equivalent pairs\n", len(pairs))

	if len(pairs) == 0 {
		logf("[main] No equivalent markets found.\n")
		if jsonMode {
			fmt.Println(string(mustJSON(map[string]any{"matches": []any{}, "summary": summarizePairs(pairs)})))
		}
		return nil
	}

	// ── 4. News (optional) ──────────────────────────────────────────────────
	newsEnabled := *newsFlag || cfg.NewsEnabled
	pairLimit := len(pairs)
	if *maxPairs > 0 && pairLimit > *maxPairs {
		pairLimit = *maxPairs
	}

	var pairNews []*news.MarketNews
	if newsEnabled && len(pairs) > 0 {
		logf("\n[main] Fetching related news for %d pairs...\n", pairLimit)
		newsFetcher := news.NewFetcher(cfg.HTTPTimeout, cfg.NewsMaxArticles)
		pairNews = newsFetcher.FetchForPairs(ctx, pairs[:pairLimit])
	}

	// ── 5. Output ───────────────────────────────────────────────────────────
	r := router.New(cfg)

	var jsonMatches []jsonMatchSummary
	var jsonDecisions []jsonRoutingDecision

	for i := 0; i < pairLimit; i++ {
		pair := pairs[i]
		jsonMatches = append(jsonMatches, newJSONMatch(pair))

		if !runRouting {
			logf("  pair %d: %s ⇄ %s | %s (%.3f)\n    %s\n    %s\n",
				i+1, pair.MarketA.VenueID, pair.MarketB.VenueID,
				pair.Confidence, pair.CompositeScore,
				pair.MarketA.Title, pair.MarketB.Title)
		} else {
			decision := r.Route(&router.Order{
				EventTitle: pair.MarketA.Title,
				Side:       orderSide,
				SizeUSD:    cfg.DefaultOrderSize,
			}, pair)

			if jsonMode {
				jsonDecisions = append(jsonDecisions, newJSONDecision(decision))
			} else {
				fmt.Println(decision.Explanation)
			}
		}

		// Print news for this pair (text mode)
		if !jsonMode && i < len(pairNews) && pairNews[i] != nil {
			printPairNews(pairNews[i])
		}
	}

	if jsonMode {
		output := map[string]any{
			"mode":              *mode,
			"matches":           jsonMatches,
			"routing_decisions": jsonDecisions,
			"summary":           summarizePairs(pairs),
		}
		if len(pairNews) > 0 {
			jsonNewsItems := make([]any, len(pairNews))
			for i, mn := range pairNews {
				jsonNewsItems[i] = mn
			}
			output["news"] = jsonNewsItems
		}
		fmt.Println(string(mustJSON(output)))
	}

	return nil
}

func printPairNews(mn *news.MarketNews) {
	if mn.Error != "" && len(mn.Articles) == 0 {
		return // silently skip failed news fetches
	}
	fmt.Println("  ── Related News ──────────────────────────")
	fmt.Printf("     Query: %q\n", mn.Query)
	if len(mn.Articles) == 0 {
		fmt.Println("     (no articles found)")
	}
	for j, article := range mn.Articles {
		age := formatAge(article.PublishedAt)
		src := article.Source
		if src == "" {
			src = "Unknown"
		}
		fmt.Printf("     %d. %s — %s (%s)\n", j+1, article.Title, src, age)
		fmt.Printf("        %s\n", article.URL)
	}
	fmt.Println()
}

func formatAge(t time.Time) string {
	if t.IsZero() {
		return "unknown time"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ─── JSON helpers ───────────────────────────────────────────────────────────

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}

type runSummary struct {
	Pairs    int `json:"pairs"`
	Match    int `json:"match"`
	Probable int `json:"probable"`
}

type marketSummary struct {
	VenueID       string `json:"venue_id"`
	VenueMarketID string `json:"venue_market_id"`
	Title         string `json:"title"`
	Resolution    string `json:"resolution_date,omitempty"`
}

type jsonMatchSummary struct {
	Confidence     string        `json:"confidence"`
	CompositeScore float64       `json:"composite_score"`
	FuzzyScore     float64       `json:"fuzzy_score"`
	Explanation    string        `json:"explanation"`
	MarketA        marketSummary `json:"market_a"`
	MarketB        marketSummary `json:"market_b"`
}

type jsonVenueScore struct {
	VenueID        string  `json:"venue_id"`
	TotalScore     float64 `json:"total_score"`
	PriceScore     float64 `json:"price_score"`
	LiquidityScore float64 `json:"liquidity_score"`
	SpreadScore    float64 `json:"spread_score"`
}

type jsonRoutingDecision struct {
	SelectedVenue string           `json:"selected_venue"`
	SelectedScore float64          `json:"selected_score"`
	VenueScores   []jsonVenueScore `json:"venue_scores"`
	Explanation   string           `json:"explanation"`
}

func summarizePairs(pairs []*matcher.MatchResult) runSummary {
	s := runSummary{Pairs: len(pairs)}
	for _, p := range pairs {
		switch p.Confidence {
		case matcher.ConfidenceMatch:
			s.Match++
		case matcher.ConfidenceProbable:
			s.Probable++
		}
	}
	return s
}

func newJSONMatch(p *matcher.MatchResult) jsonMatchSummary {
	return jsonMatchSummary{
		Confidence:     string(p.Confidence),
		CompositeScore: p.CompositeScore,
		FuzzyScore:     p.FuzzyScore,
		Explanation:    p.Explanation,
		MarketA:        newJSONMarket(p.MarketA),
		MarketB:        newJSONMarket(p.MarketB),
	}
}

func newJSONMarket(m *models.CanonicalMarket) marketSummary {
	res := ""
	if m.ResolutionDate != nil {
		res = m.ResolutionDate.Format(time.RFC3339)
	}
	return marketSummary{
		VenueID:       string(m.VenueID),
		VenueMarketID: m.VenueMarketID,
		Title:         m.Title,
		Resolution:    res,
	}
}

// runIndexedMatching uses the SQLite index for FTS-based candidate discovery.
// Much faster and more comprehensive than live API cross-search.
func runIndexedMatching(_ context.Context, _ *config.Config, mtch *matcher.Matcher, dbPath string, topN int, logf func(string, ...any)) ([]*matcher.MatchResult, error) {
	store, err := storage.NewStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening index: %w", err)
	}
	defer store.Close()

	stats, _ := store.GetStats()
	logf("[main] Using indexed mode: %d markets (%d polymarket, %d kalshi)\n",
		stats.Total, stats.ByVenue["polymarket"], stats.ByVenue["kalshi"])

	if stats.Total == 0 {
		return nil, fmt.Errorf("index is empty — run `go run ./cmd/indexer` first")
	}

	// Load all Polymarket markets (lite — no raw_payload for memory efficiency)
	polyMarkets, err := store.GetMarketsLite("polymarket")
	if err != nil {
		return nil, fmt.Errorf("loading polymarket markets: %w", err)
	}
	logf("[main] Loaded %d Polymarket markets from index\n", len(polyMarkets))

	// For each Polymarket market, FTS search for Kalshi candidates
	logf("[main] Searching index for cross-venue candidates (top-%d per market)...\n", topN)
	var searchResults []matcher.SearchResult
	searched := 0
	for _, poly := range polyMarkets {
		candidates, err := store.SearchByTitle(poly.Title, "polymarket", topN)
		if err != nil {
			continue
		}
		if len(candidates) > 0 {
			searchResults = append(searchResults, matcher.SearchResult{
				Source:     poly,
				Candidates: candidates,
			})
		}
		searched++
		if searched%5000 == 0 {
			logf("[main] Searched %d/%d markets, %d with candidates...\n",
				searched, len(polyMarkets), len(searchResults))
		}
	}
	logf("[main] FTS search complete: %d/%d markets have candidates\n",
		len(searchResults), len(polyMarkets))

	// Also search from top Kalshi markets (by volume) to find Polymarket matches
	kalshiTopN := 2000
	kalshiMarkets, err := store.GetTopMarketsLite("kalshi", kalshiTopN)
	if err == nil && len(kalshiMarkets) > 0 {
		logf("[main] Loaded %d Kalshi markets, searching for Polymarket candidates...\n", len(kalshiMarkets))
		kalshiSearched := 0
		for _, k := range kalshiMarkets {
			candidates, err := store.SearchByTitle(k.Title, "kalshi", topN)
			if err != nil {
				continue
			}
			if len(candidates) > 0 {
				searchResults = append(searchResults, matcher.SearchResult{
					Source:     k,
					Candidates: candidates,
				})
			}
			kalshiSearched++
			if kalshiSearched%500 == 0 {
				logf("[main] Kalshi reverse-search: %d/%d done...\n", kalshiSearched, len(kalshiMarkets))
			}
		}
		logf("[main] Also searched top %d Kalshi markets → total %d search results\n",
			len(kalshiMarkets), len(searchResults))
	}

	// Run matcher on all candidates
	pairs := mtch.FindMatchesFromSearchResults(searchResults, topN)
	logf("[main] Found %d equivalent pairs\n", len(pairs))
	return pairs, nil
}

// runLiveMatching fetches from live APIs and cross-searches (original behavior).
func runLiveMatching(ctx context.Context, cfg *config.Config, mtch *matcher.Matcher, numMarkets int, logf func(string, ...any)) ([]*matcher.MatchResult, error) {
	norm := normalizer.New(cfg)
	polyClient := polymarket.New(cfg.HTTPTimeout, cfg.PolymarketSearchAPI, numMarkets)
	kalshiClient := kalshi.New(cfg.KalshiAPIKey, cfg.HTTPTimeout, cfg.KalshiSearchAPI, numMarkets)

	logf("[main] Fetching top %d markets from each venue...\n", numMarkets)

	polyRaw, err := polyClient.FetchMarkets(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching polymarket: %w", err)
	}
	polyMarkets, err := norm.Normalize(ctx, polyRaw)
	if err != nil {
		return nil, fmt.Errorf("normalizing polymarket: %w", err)
	}
	logf("[main] Polymarket: %d markets\n", len(polyMarkets))

	kalshiRaw, err := kalshiClient.FetchMarkets(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching kalshi: %w", err)
	}
	kalshiMarkets, err := norm.Normalize(ctx, kalshiRaw)
	if err != nil {
		return nil, fmt.Errorf("normalizing kalshi: %w", err)
	}
	logf("[main] Kalshi: %d markets\n", len(kalshiMarkets))

	pool := &matcher.CrossSearchWorkerPool{
		Concurrency:         3,
		DelayBetweenQueries: 200 * time.Millisecond,
	}

	logf("\n[main] Searching Kalshi for each Polymarket market...\n")
	polyToKalshi := pool.RunCrossSearch(ctx, polyMarkets, func(ctx context.Context, query string) ([]*models.CanonicalMarket, error) {
		raw, err := kalshiClient.FetchMarketsByQuery(ctx, query)
		if err != nil {
			return nil, err
		}
		return norm.Normalize(ctx, raw)
	}, 10)

	logf("\n[main] Searching Polymarket for each Kalshi market...\n")
	kalshiToPoly := pool.RunCrossSearch(ctx, kalshiMarkets, func(ctx context.Context, query string) ([]*models.CanonicalMarket, error) {
		raw, err := polyClient.FetchMarketsByQuery(ctx, query)
		if err != nil {
			return nil, err
		}
		return norm.Normalize(ctx, raw)
	}, 10)

	allResults := append(polyToKalshi, kalshiToPoly...)
	pairs := mtch.FindEquivalentPairsFromSearch(ctx, allResults, "")
	logf("[main] Found %d equivalent pairs\n", len(pairs))
	return pairs, nil
}

func newJSONDecision(d *router.RoutingDecision) jsonRoutingDecision {
	scores := make([]jsonVenueScore, 0, len(d.AllScores))
	for _, s := range d.AllScores {
		scores = append(scores, jsonVenueScore{
			VenueID:        string(s.Market.VenueID),
			TotalScore:     s.TotalScore,
			PriceScore:     s.PriceScore,
			LiquidityScore: s.LiquidityScore,
			SpreadScore:    s.SpreadScore,
		})
	}
	return jsonRoutingDecision{
		SelectedVenue: string(d.SelectedVenue.VenueID),
		SelectedScore: d.FinalScore,
		VenueScores:   scores,
		Explanation:   d.Explanation,
	}
}
