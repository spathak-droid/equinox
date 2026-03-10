// Command equinox is the entry point for the Equinox cross-venue routing prototype.
//
// Usage:
//
//	OPENAI_API_KEY=sk-... KALSHI_API_KEY=... go run ./cmd/equinox
//
// The system will:
//  1. Fetch markets from Polymarket and Kalshi
//  2. Normalize each market into a CanonicalMarket
//  3. Enrich with embeddings (if OPENAI_API_KEY is set)
//  4. Detect equivalent market pairs using the multi-stage matcher
//  5. Simulate routing decisions for each matched pair
//  6. Print a structured log of all decisions to stdout
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/normalizer"
	"github.com/equinox/internal/router"
	"github.com/equinox/internal/venues"
	"github.com/equinox/internal/venues/kalshi"
	"github.com/equinox/internal/venues/polymarket"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("equinox: %v", err)
	}
}

func run() error {
	mode := flag.String("mode", "route", "run mode: route or match")
	side := flag.String("side", string(router.SideYes), "order side for routing: YES or NO")
	maxPairs := flag.Int("max-pairs", 10, "maximum pairs to print/route")
	output := flag.String("output", "text", "output format: text or json")
	mock := flag.Bool("mock", false, "use local fixtures in testdata instead of live venue APIs")
	mockPath := flag.String("mock-path", "testdata/markets.mock.json", "path to mock fixtures file")
	outputFile := flag.String("output-file", "", "write JSON output to this file (JSON mode only)")
	flag.Parse()

	orderSide := router.SideYes
	switch strings.ToUpper(*side) {
	case string(router.SideYes), "Y", "BUYYES", "BUY_YES":
		orderSide = router.SideYes
	case string(router.SideNo), "N", "BUYNO", "BUY_NO":
		orderSide = router.SideNo
	default:
		return fmt.Errorf("invalid side %q: use YES or NO", *side)
	}
	if *maxPairs < 0 {
		*maxPairs = 0
	}
	runRouting := strings.ToLower(*mode) == "route"
	runMatchingOnly := strings.ToLower(*mode) == "match" || strings.ToLower(*mode) == "match-only"
	if !runRouting && !runMatchingOnly {
		return fmt.Errorf("invalid mode %q: use route or match", *mode)
	}
	if strings.ToLower(*output) != "text" && strings.ToLower(*output) != "json" {
		return fmt.Errorf("invalid output %q: use text or json", *output)
	}
	jsonMode := strings.ToLower(*output) == "json"
	logf := func(format string, args ...any) {
		if !jsonMode {
			fmt.Printf(format, args...)
		}
	}

	// ── 1. Load config ────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// ── 2. Initialize venues ──────────────────────────────────────────────────
	polyClient := polymarket.New(cfg.HTTPTimeout)
	kalshiClient := kalshi.New(cfg.KalshiAPIKey, cfg.HTTPTimeout)

	venueClients := []venues.Venue{polyClient, kalshiClient}
	searchableVenues := map[models.VenueID]venues.SearchableVenue{
		models.VenuePolymarket: polyClient,
		models.VenueKalshi:     kalshiClient,
	}

	// ── 3. Fetch & normalize all markets ─────────────────────────────────────
	norm := normalizer.New(cfg)
	marketsByVenue := map[models.VenueID][]*models.CanonicalMarket{}
	var allMarkets []*models.CanonicalMarket

	for _, v := range venueClients {
		logf("\n[main] Fetching markets from %s...\n", v.ID())

		var raw []*venues.RawMarket
		var err error
		if *mock {
			raw, err = loadMockMarkets(*mockPath, v.ID())
		} else {
			raw, err = v.FetchMarkets(ctx)
		}

		if err != nil {
			// Non-fatal: if one venue is unavailable/missing fixture, continue with others.
			// This is an explicit tradeoff: partial data is better than a full abort.
			logf("[main] WARNING: failed to fetch from %s: %v — skipping\n", v.ID(), err)
			continue
		}
		logf("[main] Fetched %d raw markets from %s\n", len(raw), v.ID())

		canonical, err := norm.Normalize(ctx, raw)
		if err != nil {
			return fmt.Errorf("normalizing markets from %s: %w", v.ID(), err)
		}
		logf("[main] Normalized %d markets from %s\n", len(canonical), v.ID())
		marketsByVenue[v.ID()] = canonical
		allMarkets = append(allMarkets, canonical...)
	}

	if len(allMarkets) == 0 {
		return fmt.Errorf("no markets fetched — check venue connectivity and API keys")
	}

	logf("\n[main] Total canonical markets: %d\n", len(allMarkets))

	// ── 4. Match equivalent markets ───────────────────────────────────────────
	var openaiClient *openai.Client
	if cfg.OpenAIAPIKey != "" {
		openaiClient = openai.NewClient(cfg.OpenAIAPIKey)
	}
	mtch := matcher.New(cfg, openaiClient)

	var pairs []*matcher.MatchResult

	// Use query-based cross-search when not in mock mode and we have multiple searchable venues.
	// This is dramatically more effective than brute-force O(n²) comparison.
	useSearch := !*mock && len(searchableVenues) >= 2 && len(marketsByVenue) >= 2
	if useSearch {
		logf("\n[main] Running query-based cross-search matching...\n")

		pool := &matcher.CrossSearchWorkerPool{
			Concurrency:         3,                       // low concurrency to avoid 429 rate limits
			DelayBetweenQueries: 300 * time.Millisecond,  // rate limit between queries
		}
		var allSearchResults []matcher.SearchResult

		// For each venue, search the other venues using market titles as queries
		for srcVenue, srcMarkets := range marketsByVenue {
			// Diversify source markets: group similar titles (e.g. 30 NHL teams)
			// into a single representative query each
			searchBatch := matcher.DiversifySourceMarkets(srcMarkets, 50)

			for tgtVenue, tgtSearchable := range searchableVenues {
				if srcVenue == tgtVenue {
					continue
				}

				logf("[main] Cross-searching: %d %s markets → %s search API (from %d total)\n",
					len(searchBatch), srcVenue, tgtVenue, len(srcMarkets))

				// Build search function that searches + normalizes
				searchFn := func(ctx context.Context, query string) ([]*models.CanonicalMarket, error) {
					raw, err := tgtSearchable.FetchMarketsByQuery(ctx, query)
					if err != nil {
						return nil, err
					}
					if len(raw) == 0 {
						return nil, nil
					}
					return norm.Normalize(ctx, raw)
				}

				results := pool.RunCrossSearch(ctx, searchBatch, searchFn, 10)
				allSearchResults = append(allSearchResults, results...)
			}
		}

		logf("[main] Cross-search produced %d source→candidate groups\n", len(allSearchResults))
		pairs = mtch.FindEquivalentPairsFromSearch(ctx, allSearchResults)
	}

	// Fall back to brute-force if search found nothing, or in mock mode
	if len(pairs) == 0 {
		if useSearch {
			logf("[main] Cross-search found no matches, falling back to brute-force...\n")
		} else {
			logf("\n[main] Running brute-force equivalence detection...\n")
		}
		pairs = mtch.FindEquivalentPairs(ctx, allMarkets)
	}

	logf("[main] Found %d equivalent pairs\n", len(pairs))
	if len(pairs) == 0 {
		if jsonMode {
			payload := struct {
				Mode      string         `json:"mode"`
				Matches   []jsonMatchSummary `json:"matches"`
				Summary   runSummary     `json:"summary"`
			}{
				Mode:    strings.ToLower(*mode),
				Matches: nil,
				Summary: summarizePairs(pairs),
			}
			data, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				return fmt.Errorf("marshalling zero-match output: %w", err)
			}
			if err := writeJSONOutput(*outputFile, data); err != nil {
				return err
			}
			return nil
		}
		logf("[main] No equivalent markets found. Consider:\n")
		logf("       - Lowering MATCH_THRESHOLD (current: %f)\n", cfg.MatchThreshold)
		logf("       - Increasing MAX_DATE_DELTA_DAYS (current: %d)\n", cfg.MaxDateDeltaDays)
		logf("       - Setting OPENAI_API_KEY to enable embedding matching\n")
		return nil
	}

	// ── 5. Simulate routing decisions ─────────────────────────────────────────
	logf("\n[main] Simulating routing decisions...\n")
	r := router.New(cfg)

	var jsonMatches []jsonMatchSummary
	var jsonDecisions []jsonRoutingDecision
	pairLimit := len(pairs)
	if *maxPairs > 0 && pairLimit > *maxPairs {
		pairLimit = *maxPairs
	}
	if runMatchingOnly && !runRouting {
		for i := 0; i < pairLimit; i++ {
			pair := pairs[i]
			logf("[main] %-4s pair %d: %s ⇄ %s | %s (%.3f)\n",
				pair.Confidence, i+1, pair.MarketA.VenueID, pair.MarketB.VenueID,
				pair.Confidence, pair.CompositeScore)
			jsonMatches = append(jsonMatches, newJSONMatch(pair))
		}
		payload := struct {
			Mode      string            `json:"mode"`
			Matches   []jsonMatchSummary `json:"matches"`
			Summary   runSummary        `json:"summary"`
		}{
			Mode:    strings.ToLower(*mode),
			Matches: jsonMatches,
			Summary: summarizePairs(pairs),
		}
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return fmt.Errorf("marshalling match output: %w", err)
		}
		if err := writeJSONOutput(*outputFile, data); err != nil {
			return err
		}
		return nil
	}

	for i := 0; i < pairLimit; i++ {
		pair := pairs[i]
		jsonMatches = append(jsonMatches, newJSONMatch(pair))

		order := &router.Order{
			EventTitle: pair.MarketA.Title,
			Side:       orderSide,
			SizeUSD:    cfg.DefaultOrderSize,
		}

		decision := r.Route(order, pair)
		if jsonMode {
			jsonDecisions = append(jsonDecisions, newJSONDecision(decision))
		} else {
			fmt.Println(decision.Explanation)
		}
	}

	if jsonMode {
		payload := struct {
			Mode             string              `json:"mode"`
			OrderSide        string              `json:"order_side"`
			MaxPairs         int                 `json:"max_pairs"`
			Matches          []jsonMatchSummary  `json:"matches"`
			RoutingDecisions []jsonRoutingDecision `json:"routing_decisions"`
			Summary          runSummary          `json:"summary"`
		}{
			Mode:             strings.ToLower(*mode),
			OrderSide:        string(orderSide),
			MaxPairs:         *maxPairs,
			Matches:          jsonMatches,
			RoutingDecisions: jsonDecisions,
			Summary:          summarizePairs(pairs),
		}
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return fmt.Errorf("marshalling routing output: %w", err)
		}
		if err := writeJSONOutput(*outputFile, data); err != nil {
			return err
		}
		return nil
	}

	// ── 6. Summary ────────────────────────────────────────────────────────────
	if !jsonMode {
		printSummary(allMarkets, pairs)
	}

	return nil
}

type fixtureMarket struct {
	VenueID       string          `json:"venue_id"`
	VenueMarketID string          `json:"venue_market_id"`
	Payload       json.RawMessage `json:"payload"`
}

type venueIdProbe struct {
	VenueMarketID string `json:"venue_market_id"`
	ID            string `json:"id"`
	VenueID       string `json:"venue_id"`
	Ticker        string `json:"ticker"`
}

func loadMockMarkets(path string, venue models.VenueID) ([]*venues.RawMarket, error) {
	rawFixture, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading mock fixture %q: %w", path, err)
	}

	// Supported format #1:
	// {
	//   "polymarket": [{...raw venue payload...}],
	//   "kalshi": [{...raw venue payload...}]
	// }
	var byVenue map[string]json.RawMessage
	if err := json.Unmarshal(rawFixture, &byVenue); err == nil {
		if data, ok := byVenue[string(venue)]; ok {
			var rawItems []json.RawMessage
			if err := json.Unmarshal(data, &rawItems); err != nil {
				return nil, fmt.Errorf("mock fixture venue %q is not an array: %w", venue, err)
			}
			return toRawMarkets(venue, rawItems)
		}
		// If keys exist and this venue is absent, return empty instead of failing.
		return nil, nil
	}

	// Supported format #2:
	// [
	//   {"venue_id":"polymarket","venue_market_id":"...", "payload":{...}},
	//   ...
	// ]
	var list []fixtureMarket
	if err := json.Unmarshal(rawFixture, &list); err != nil {
		return nil, fmt.Errorf("mock fixture parse error for %q: expected map by venue or list entries: %w", path, err)
	}

	var result []json.RawMessage
	for _, item := range list {
		if item.VenueID != "" && item.VenueID != string(venue) {
			continue
		}
		if item.VenueID == "" && item.Payload == nil && item.VenueMarketID == "" {
			continue
		}
		if item.Payload != nil {
			result = append(result, item.Payload)
			continue
		}
		b, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("mock fixture marshal for %q failed: %w", venue, err)
		}
		result = append(result, b)
	}
	return toRawMarkets(venue, result)
}

func toRawMarkets(venue models.VenueID, rawItems []json.RawMessage) ([]*venues.RawMarket, error) {
	if len(rawItems) == 0 {
		return nil, nil
	}
	result := make([]*venues.RawMarket, 0, len(rawItems))
	for i, item := range rawItems {
		var probe venueIdProbe
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, fmt.Errorf("mock payload parse error for %q item %d: %w", venue, i, err)
		}
		marketID := probe.VenueMarketID
		if marketID == "" {
			marketID = probe.ID
		}
		if marketID == "" {
			marketID = probe.Ticker
		}
		if marketID == "" {
			marketID = fmt.Sprintf("%s-fallback-%d", venue, i)
		}
		result = append(result, &venues.RawMarket{
			VenueID:       venue,
			VenueMarketID: marketID,
			Payload:       item,
		})
	}
	return result, nil
}

// printSummary writes a structured summary of the run to stdout.
func printSummary(markets []*models.CanonicalMarket, pairs []*matcher.MatchResult) {
	venueCount := map[models.VenueID]int{}
	for _, m := range markets {
		venueCount[m.VenueID]++
	}

	matchCount := 0
	probableCount := 0
	for _, p := range pairs {
		switch p.Confidence {
		case matcher.ConfidenceMatch:
			matchCount++
		case matcher.ConfidenceProbable:
			probableCount++
		}
	}

	fmt.Println("\n═══════════════════════════════════════════════════════════")
	fmt.Println("EQUINOX RUN SUMMARY")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("Markets fetched:")
	for venue, count := range venueCount {
		fmt.Printf("  %-20s %d\n", venue, count)
	}
	fmt.Printf("  %-20s %d\n", "TOTAL", len(markets))
	fmt.Println()
	fmt.Printf("Equivalent pairs found:  %d (MATCH) + %d (PROBABLE)\n", matchCount, probableCount)
	fmt.Println("═══════════════════════════════════════════════════════════")

	// Exit code 0 even with no matches — this is expected in dry runs
	os.Exit(0)
}

func writeJSONOutput(path string, data []byte) error {
	if path != "" {
		if err := os.WriteFile(path, append([]byte{}, data...), 0o644); err != nil {
			return fmt.Errorf("writing json output to %q: %w", path, err)
		}
		return nil
	}
	fmt.Println(string(data))
	return nil
}

type runSummary struct {
	Mode     string `json:"mode"`
	Pairs    int    `json:"pairs"`
	Match    int    `json:"match"`
	Probable int    `json:"probable"`
	NoMatch  int    `json:"no_match"`
}

type marketSummary struct {
	VenueID       string `json:"venue_id"`
	VenueMarketID string `json:"venue_market_id"`
	Title         string `json:"title"`
	Resolution    string `json:"resolution_date,omitempty"`
	Status        string `json:"status"`
}

type jsonMatchSummary struct {
	Confidence     string        `json:"confidence"`
	CompositeScore float64       `json:"composite_score"`
	FuzzyScore     float64       `json:"fuzzy_score"`
	EmbeddingScore float64       `json:"embedding_score"`
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
	Explanation    string  `json:"explanation"`
}

type jsonRoutingDecision struct {
	Order struct {
		EventTitle string  `json:"event_title"`
		Side       string  `json:"side"`
		SizeUSD    float64 `json:"size_usd"`
	} `json:"order"`
	MatchedConfidence string           `json:"matched_confidence"`
	MatchedComposite  float64          `json:"matched_composite"`
	SelectedVenue     string           `json:"selected_venue"`
	SelectedScore     float64          `json:"selected_score"`
	VenueScores       []jsonVenueScore `json:"venue_scores"`
	Explanation       string           `json:"explanation"`
}

func summarizePairs(pairs []*matcher.MatchResult) runSummary {
	s := runSummary{
		Mode: "route",
		Pairs: len(pairs),
	}
	for _, p := range pairs {
		switch p.Confidence {
		case matcher.ConfidenceMatch:
			s.Match++
		case matcher.ConfidenceProbable:
			s.Probable++
		default:
			s.NoMatch++
		}
	}
	return s
}

func newJSONMatch(p *matcher.MatchResult) jsonMatchSummary {
	return jsonMatchSummary{
		Confidence:     string(p.Confidence),
		CompositeScore: p.CompositeScore,
		FuzzyScore:     p.FuzzyScore,
		EmbeddingScore: p.EmbeddingScore,
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
		Status:        string(m.Status),
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
			Explanation:    s.Explanation,
		})
	}
	return jsonRoutingDecision{
		Order: struct {
			EventTitle string  `json:"event_title"`
			Side       string  `json:"side"`
			SizeUSD    float64 `json:"size_usd"`
		}{
			EventTitle: d.Order.EventTitle,
			Side:       string(d.Order.Side),
			SizeUSD:    d.Order.SizeUSD,
		},
		MatchedConfidence: string(d.MatchedPair.Confidence),
		MatchedComposite:  d.MatchedPair.CompositeScore,
		SelectedVenue:     string(d.SelectedVenue.VenueID),
		SelectedScore:     d.FinalScore,
		VenueScores:       scores,
		Explanation:       d.Explanation,
	}
}
