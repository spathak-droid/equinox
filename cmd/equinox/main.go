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
	"github.com/equinox/internal/normalizer"
	"github.com/equinox/internal/router"
	"github.com/equinox/internal/venues/kalshi"
	"github.com/equinox/internal/venues/polymarket"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatal("Error loading .env file")
	}
	if err := run(); err != nil {
		log.Fatalf("equinox: %v", err)
	}
}

func run() error {
	mode := flag.String("mode", "route", "run mode: route or match")
	side := flag.String("side", string(router.SideYes), "order side: YES or NO")
	maxPairs := flag.Int("max-pairs", 10, "maximum pairs to output")
	numMarkets := flag.Int("n", 10, "number of markets to fetch from each venue")
	output := flag.String("output", "text", "output format: text or json")
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

	norm := normalizer.New(cfg)
	polyClient := polymarket.New(cfg.HTTPTimeout, cfg.PolymarketSearchAPI, *numMarkets)
	kalshiClient := kalshi.New(cfg.KalshiAPIKey, cfg.HTTPTimeout, cfg.KalshiSearchAPI, *numMarkets)

	// ── 1. Fetch top N from each venue ──────────────────────────────────────
	logf("[main] Fetching top %d markets from each venue...\n", *numMarkets)

	polyRaw, err := polyClient.FetchMarkets(ctx)
	if err != nil {
		return fmt.Errorf("fetching polymarket: %w", err)
	}
	polyMarkets, err := norm.Normalize(ctx, polyRaw)
	if err != nil {
		return fmt.Errorf("normalizing polymarket: %w", err)
	}
	logf("[main] Polymarket: %d markets\n", len(polyMarkets))

	kalshiRaw, err := kalshiClient.FetchMarkets(ctx)
	if err != nil {
		return fmt.Errorf("fetching kalshi: %w", err)
	}
	kalshiMarkets, err := norm.Normalize(ctx, kalshiRaw)
	if err != nil {
		return fmt.Errorf("normalizing kalshi: %w", err)
	}
	logf("[main] Kalshi: %d markets\n", len(kalshiMarkets))

	// ── 2. Cross-search: Poly→Kalshi and Kalshi→Poly ────────────────────────
	mtch := matcher.New(cfg)
	pool := &matcher.CrossSearchWorkerPool{
		Concurrency:         3,
		DelayBetweenQueries: 200 * time.Millisecond,
	}

	// Poly markets → search Kalshi
	logf("\n[main] Searching Kalshi for each Polymarket market...\n")
	polyToKalshi := pool.RunCrossSearch(ctx, polyMarkets, func(ctx context.Context, query string) ([]*models.CanonicalMarket, error) {
		raw, err := kalshiClient.FetchMarketsByQuery(ctx, query)
		if err != nil {
			return nil, err
		}
		return norm.Normalize(ctx, raw)
	}, 10)

	// Kalshi markets → search Polymarket
	logf("\n[main] Searching Polymarket for each Kalshi market...\n")
	kalshiToPoly := pool.RunCrossSearch(ctx, kalshiMarkets, func(ctx context.Context, query string) ([]*models.CanonicalMarket, error) {
		raw, err := polyClient.FetchMarketsByQuery(ctx, query)
		if err != nil {
			return nil, err
		}
		return norm.Normalize(ctx, raw)
	}, 10)

	// Combine search results from both directions
	allResults := append(polyToKalshi, kalshiToPoly...)

	// ── 3. Score and match ──────────────────────────────────────────────────
	pairs := mtch.FindEquivalentPairsFromSearch(ctx, allResults)
	logf("\n[main] Found %d equivalent pairs\n", len(pairs))

	if len(pairs) == 0 {
		logf("[main] No equivalent markets found.\n")
		if jsonMode {
			fmt.Println(string(mustJSON(map[string]any{"matches": []any{}, "summary": summarizePairs(pairs)})))
		}
		return nil
	}

	// ── 4. Output ───────────────────────────────────────────────────────────
	r := router.New(cfg)
	pairLimit := len(pairs)
	if *maxPairs > 0 && pairLimit > *maxPairs {
		pairLimit = *maxPairs
	}

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
			continue
		}

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

	if jsonMode {
		fmt.Println(string(mustJSON(map[string]any{
			"mode":              *mode,
			"matches":           jsonMatches,
			"routing_decisions": jsonDecisions,
			"summary":           summarizePairs(pairs),
		})))
	}

	return nil
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
