// Command equinox-ui serves a local web UI for searching and comparing
// equivalent prediction markets across Polymarket and Kalshi.
//
// Usage:
//
//	OPENAI_API_KEY=sk-... go run ./cmd/equinox-ui
//
// Then open http://localhost:8080 in your browser.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
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
	"github.com/joho/godotenv"
)

const uiPerVenueLimit = 100
const maxDisplayPairs = 20

// resultCache caches pipeline results for 2 minutes so the /stream endpoint
// can run the pipeline once and the subsequent GET / serves instantly.
var resultCache sync.Map // map[string]*cachedResult

type cachedResult struct {
	data      *PageData
	expiresAt time.Time
}

// progressEvent is an SSE payload sent during the search pipeline.
type progressEvent struct {
	Type  string    `json:"type"`            // "step" | "result" | "pair" | "done" | "error"
	Msg   string    `json:"msg"`
	Count int       `json:"count,omitempty"` // market / pair count for "result" events
	Venue string    `json:"venue,omitempty"` // "polymarket" | "kalshi" | ""
	Pair  *PairView `json:"pair,omitempty"`  // streamed pair card data
	Index int       `json:"index,omitempty"` // pair index (1-based)
}

// PageData is passed to the HTML template.
type PageData struct {
	SearchQuery      string
	Pairs            []PairView
	VenueCounts      map[models.VenueID]int
	MatchCount       int
	ProbableCount    int
	DiagnosisMessage string
	HasQuery         bool
	DeepSearch       bool
}

// PairView is a single matched pair ready for rendering.
type PairView struct {
	MarketA        MarketView `json:"market_a"`
	MarketB        MarketView `json:"market_b"`
	Confidence     string     `json:"confidence"`
	FuzzyScore     float64    `json:"fuzzy_score"`
	EmbeddingScore float64    `json:"embedding_score"`
	CompositeScore float64    `json:"composite_score"`
	Explanation    string     `json:"explanation"`
	SelectedVenue  string     `json:"selected_venue"`
	RoutingReason  string     `json:"routing_reason"`
}

// MarketView is a single market ready for rendering.
type MarketView struct {
	Venue              string  `json:"venue"`
	VenueMarketID      string  `json:"venue_market_id"`
	VenueYesTokenID    string  `json:"venue_yes_token_id,omitempty"`
	Title              string  `json:"title"`
	Category           string  `json:"category"`
	Status             string  `json:"status"`
	Description        string  `json:"description"`
	Tags               string  `json:"tags"`
	ImageURL           string  `json:"image_url"`
	YesPrice           float64 `json:"yes_price"`
	Liquidity          float64 `json:"liquidity"`
	Spread             float64 `json:"spread"`
	ResolutionDate     string  `json:"resolution_date"`
	CreatedAt          string  `json:"created_at"`
	UpdatedAt          string  `json:"updated_at"`
	Volume24h          float64 `json:"volume_24h"`
	OpenInterest       float64 `json:"open_interest"`
	ResolutionRaw      string  `json:"resolution_raw"`
	RawPayloadB64      string  `json:"raw_payload_b64"`
	VenueLink          string  `json:"venue_link"`
	VenueSearchLink    string  `json:"venue_search_link"`
	VenueSearchLinkAlt string  `json:"venue_search_link_alt"`
}

func main() {
	// .env is optional — Railway (and other cloud hosts) inject vars directly.
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	// Create venue clients once. The Kalshi client has no maxMarkets cap so
	// its events index covers all series. The events cache (5 min TTL) is
	// shared across all search requests served by this process.
	kalshiClient := kalshi.New(cfg.KalshiAPIKey, cfg.HTTPTimeout, cfg.KalshiSearchAPI)
	polyClient := polymarket.New(cfg.HTTPTimeout, cfg.PolymarketSearchAPI, uiPerVenueLimit)

	// v1 search API needs no prewarming — queries go directly to the search endpoint.

	fmt.Println("[equinox-ui] Serving at http://localhost:8080")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		deepSearch := r.URL.Query().Get("more") == "1"
		cacheKey := query
		if deepSearch {
			cacheKey = query + "|more=1"
		}

		var data *PageData
		if query == "" {
			data = &PageData{VenueCounts: map[models.VenueID]int{}}
		} else {
			// Check short-lived cache populated by /stream
			if v, ok := resultCache.Load(cacheKey); ok {
				cr := v.(*cachedResult)
				if time.Now().Before(cr.expiresAt) {
					data = cr.data
				} else {
					resultCache.Delete(cacheKey)
				}
			}
			if data == nil {
				fmt.Printf("[equinox-ui] Running search pipeline q=%q deep=%t\n", query, deepSearch)
				data, err = runSearchPipelineWithProgress(cfg, kalshiClient, polyClient, query, deepSearch, nil)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := pageTmpl.Execute(w, data); err != nil {
			fmt.Printf("[equinox-ui] ERROR: rendering template: %v\n", err)
		}
	})

	// /stream runs the search pipeline and emits SSE progress events, then
	// caches the result so the subsequent GET / is served instantly.
	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		deepSearch := r.URL.Query().Get("more") == "1"
		cacheKey := query
		if deepSearch {
			cacheKey = query + "|more=1"
		}
		if query == "" {
			http.Error(w, "missing q", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		var mu sync.Mutex
		emit := func(evt progressEvent) {
			b, _ := json.Marshal(evt)
			mu.Lock()
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
			mu.Unlock()
		}

		data, pipelineErr := runSearchPipelineWithProgress(cfg, kalshiClient, polyClient, query, deepSearch, emit)
		if pipelineErr != nil {
			emit(progressEvent{Type: "error", Msg: pipelineErr.Error()})
			return
		}

		// Cache result so the redirect GET / is instant
		resultCache.Store(cacheKey, &cachedResult{
			data:      data,
			expiresAt: time.Now().Add(2 * time.Minute),
		})
		emit(progressEvent{Type: "done"})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("[equinox-ui] Listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// maxPages is the maximum number of API pages to fetch per venue per query.
// After maxPages we stop paginating regardless of match count.
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
		m     *models.CanonicalMarket
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
		poly      *models.CanonicalMarket
		kalshi    *models.CanonicalMarket
		polyQ     float64
		kalshiQ   float64
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

// buildPageData takes pre-computed match pairs and builds the PageData for the template.
func buildPageData(cfg *config.Config, ctx context.Context, m *matcher.Matcher, allMarkets []*models.CanonicalMarket, pairs []*matcher.MatchResult, venueCounts map[models.VenueID]int, query string) (*PageData, error) {
	if len(pairs) > maxDisplayPairs {
		pairs = pairs[:maxDisplayPairs]
	}

	var diagnosisMsg string
	if len(pairs) == 0 {
		rejected := m.TopRejectedPairs(allMarkets, 5)
		for i, rj := range rejected {
			fmt.Printf("[equinox-ui] reject #%d score=%.3f fuzzy=%.3f emb=%.3f | A=%q | B=%q | reason=%s\n",
				i+1, rj.CompositeScore, rj.FuzzyScore, rj.EmbeddingScore,
				rj.MarketA.Title, rj.MarketB.Title, rj.Explanation)
		}
		diagnosisMsg = buildDiagnosis(venueCounts, rejected)
	}

	r := router.New(cfg)
	var pairViews []PairView
	for _, p := range pairs {
		order := &router.Order{
			EventTitle: p.MarketA.Title,
			Side:       router.SideYes,
			SizeUSD:    cfg.DefaultOrderSize,
		}
		decision := r.Route(order, p)
		embScore := p.EmbeddingScore
		if embScore < 0 {
			embScore = 0
		}
		pairViews = append(pairViews, PairView{
			MarketA:        toMarketView(p.MarketA),
			MarketB:        toMarketView(p.MarketB),
			Confidence:     string(p.Confidence),
			FuzzyScore:     p.FuzzyScore,
			EmbeddingScore: embScore,
			CompositeScore: p.CompositeScore,
			Explanation:    p.Explanation,
			SelectedVenue:  string(decision.SelectedVenue.VenueID),
			RoutingReason:  decision.Explanation,
		})
	}

	matchCount, probableCount := 0, 0
	for _, pv := range pairViews {
		switch pv.Confidence {
		case "MATCH":
			matchCount++
		case "PROBABLE_MATCH":
			probableCount++
		}
	}

	return &PageData{
		SearchQuery:      query,
		Pairs:            pairViews,
		VenueCounts:      venueCounts,
		MatchCount:       matchCount,
		ProbableCount:    probableCount,
		DiagnosisMessage: diagnosisMsg,
		HasQuery:         true,
	}, nil
}

// matchToPairView converts a single MatchResult to a PairView with routing.
func matchToPairView(cfg *config.Config, r *router.Router, p *matcher.MatchResult) PairView {
	order := &router.Order{
		EventTitle: p.MarketA.Title,
		Side:       router.SideYes,
		SizeUSD:    cfg.DefaultOrderSize,
	}
	decision := r.Route(order, p)
	embScore := p.EmbeddingScore
	if embScore < 0 {
		embScore = 0
	}
	return PairView{
		MarketA:        toMarketView(p.MarketA),
		MarketB:        toMarketView(p.MarketB),
		Confidence:     string(p.Confidence),
		FuzzyScore:     p.FuzzyScore,
		EmbeddingScore: embScore,
		CompositeScore: p.CompositeScore,
		Explanation:    p.Explanation,
		SelectedVenue:  string(decision.SelectedVenue.VenueID),
		RoutingReason:  decision.Explanation,
	}
}

// buildDiagnosis generates a human-readable explanation of why no matches were found.
func buildDiagnosis(venueCounts map[models.VenueID]int, rejected []*matcher.MatchResult) string {
	polyCount := venueCounts[models.VenuePolymarket]
	kalshiCount := venueCounts[models.VenueKalshi]

	if polyCount == 0 && kalshiCount == 0 {
		return "Neither venue returned markets for this query."
	}
	if polyCount == 0 {
		return fmt.Sprintf("Only Kalshi returned markets (%d). Polymarket had no results for this query, so no cross-venue comparison is possible.", kalshiCount)
	}
	if kalshiCount == 0 {
		return fmt.Sprintf("Only Polymarket returned markets (%d). Kalshi had no results for this query, so no cross-venue comparison is possible.", polyCount)
	}
	if len(rejected) == 0 {
		return fmt.Sprintf("Both venues returned markets (Polymarket: %d, Kalshi: %d) but no cross-venue pairs could be formed.", polyCount, kalshiCount)
	}

	datePenalized, lowSemantic := 0, 0
	var highestComposite float64
	for _, r := range rejected {
		if r.DatePenalty > 0.5 {
			datePenalized++
		}
		if r.CompositeScore < 0.35 {
			lowSemantic++
		}
		if r.CompositeScore > highestComposite {
			highestComposite = r.CompositeScore
		}
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("Both venues have markets (Polymarket: %d, Kalshi: %d) but the questions they ask are different.", polyCount, kalshiCount))
	if datePenalized > 0 && datePenalized == len(rejected) {
		parts = append(parts, "All top candidates have resolution dates far apart, suggesting the venues may be tracking different time periods for similar events.")
	} else if lowSemantic == len(rejected) {
		parts = append(parts, "The market titles have low semantic similarity \u2014 the venues appear to be asking fundamentally different questions about this topic.")
	} else {
		parts = append(parts, fmt.Sprintf("The closest pair scored %.2f (threshold: 0.45). The markets may cover different aspects of the same topic.", highestComposite))
	}
	return strings.Join(parts, " ")
}

func toMarketView(m *models.CanonicalMarket) MarketView {
	res := ""
	if m.ResolutionDate != nil {
		res = m.ResolutionDate.Format("2006-01-02")
	}
	createdAt, updatedAt := "", ""
	if !m.CreatedAt.IsZero() {
		createdAt = m.CreatedAt.Format("2006-01-02 15:04:05")
	}
	if !m.UpdatedAt.IsZero() {
		updatedAt = m.UpdatedAt.Format("2006-01-02 15:04:05")
	}
	return MarketView{
		Venue:              string(m.VenueID),
		VenueMarketID:      m.VenueMarketID,
		VenueYesTokenID:    m.VenueYesTokenID,
		Title:              m.Title,
		Category:           m.Category,
		Status:             string(m.Status),
		Description:        m.Description,
		Tags:               strings.Join(m.Tags, ", "),
		ImageURL:           m.ImageURL,
		YesPrice:           m.YesPrice,
		Liquidity:          m.Liquidity,
		Spread:             m.Spread,
		ResolutionDate:     res,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
		Volume24h:          m.Volume24h,
		OpenInterest:       m.OpenInterest,
		ResolutionRaw:      m.ResolutionCriteria,
		RawPayloadB64:      base64.StdEncoding.EncodeToString(m.RawPayload),
		VenueLink:          marketVenueLink(m),
		VenueSearchLink:    marketVenueSearchLink(m),
		VenueSearchLinkAlt: marketVenueSearchLinkAlt(m),
	}
}

func marketVenueLink(m *models.CanonicalMarket) string {
	switch m.VenueID {
	case models.VenuePolymarket:
		slug := m.VenueSlug
		if slug == "" {
			slug = m.VenueMarketID
		}
		if m.VenueEventTicker != "" {
			return "https://polymarket.com/event/" + url.PathEscape(m.VenueEventTicker) + "/" + url.PathEscape(slug)
		}
		return "https://polymarket.com/market/" + url.PathEscape(slug)
	case models.VenueKalshi:
		return marketVenueKalshiLink(m)
	default:
		return ""
	}
}

func marketVenueSearchLink(m *models.CanonicalMarket) string {
	term := url.QueryEscape(m.Title)
	switch m.VenueID {
	case models.VenuePolymarket:
		return "https://polymarket.com/markets?search=" + term
	case models.VenueKalshi:
		return "https://kalshi.com/browse?search=" + term
	default:
		return ""
	}
}

func marketVenueSearchLinkAlt(m *models.CanonicalMarket) string {
	switch m.VenueID {
	case models.VenueKalshi:
		if m.VenueMarketID == "" {
			return ""
		}
		return "https://kalshi.com/browse?search=" + url.QueryEscape(strings.ToLower(m.VenueMarketID))
	default:
		return ""
	}
}

func marketVenueKalshiLink(m *models.CanonicalMarket) string {
	seriesTicker := strings.TrimSpace(strings.ToLower(m.VenueSeriesTicker))
	eventTicker := strings.TrimSpace(strings.ToLower(m.VenueEventTicker))
	eventTitle := strings.TrimSpace(m.VenueEventTitle)
	if seriesTicker != "" && eventTicker != "" && eventTitle != "" {
		slug := kalshiTitleSlug(eventTitle)
		return "https://kalshi.com/markets/" + url.PathEscape(seriesTicker) + "/" + slug + "/" + url.PathEscape(eventTicker)
	}
	if m.Title != "" {
		return "https://kalshi.com/browse?search=" + url.QueryEscape(m.Title)
	}
	return ""
}

func kalshiTitleSlug(title string) string {
	s := strings.ToLower(title)
	var buf strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			buf.WriteRune(r)
		} else {
			buf.WriteByte('-')
		}
	}
	result := buf.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	return strings.Trim(result, "-")
}

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"pct": func(f float64) string { return fmt.Sprintf("%.1f%%", f*100) },
	"usd": func(f float64) string {
		if f == 0 {
			return "--"
		}
		if f >= 1000000 {
			return fmt.Sprintf("$%.1fM", f/1000000)
		}
		if f >= 1000 {
			return fmt.Sprintf("$%.1fK", f/1000)
		}
		return fmt.Sprintf("$%.0f", f)
	},
	"score":      func(f float64) string { return fmt.Sprintf("%.3f", f) },
	"scoreWidth": func(f float64) string { return fmt.Sprintf("%.1f%%", f*100) },
	"confClass": func(c string) string {
		switch c {
		case "MATCH":
			return "conf-match"
		case "PROBABLE_MATCH":
			return "conf-probable"
		default:
			return "conf-no"
		}
	},
	"cardClass": func(c string) string {
		switch c {
		case "MATCH":
			return "card-match"
		case "PROBABLE_MATCH":
			return "card-probable"
		default:
			return ""
		}
	},
	"confIcon": func(c string) string {
		switch c {
		case "MATCH":
			return "check_circle"
		case "PROBABLE_MATCH":
			return "help"
		default:
			return "cancel"
		}
	},
	"venueClass": func(v string) string {
		if v == "polymarket" {
			return "venue-poly"
		}
		return "venue-kalshi"
	},
	"venueIcon": func(v string) string {
		if v == "polymarket" {
			return "P"
		}
		return "K"
	},
	"bigVol": func(f float64) bool { return f >= 1000 },
	"inc": func(i int) int { return i + 1 },
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Equinox</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700;800&display=swap" rel="stylesheet">
<link href="https://fonts.googleapis.com/icon?family=Material+Icons+Round" rel="stylesheet">
<style>
:root {
  --bg-primary: #06080d;
  --bg-secondary: #0c1017;
  --bg-card: #111620;
  --bg-card-hover: #161d2a;
  --bg-elevated: #1a2233;
  --border: rgba(99, 102, 241, 0.08);
  --border-hover: rgba(99, 102, 241, 0.2);
  --text-primary: #f1f5f9;
  --text-secondary: #94a3b8;
  --text-muted: #475569;
  --accent: #818cf8;
  --accent-glow: rgba(129, 140, 248, 0.15);
  --accent-bright: #a5b4fc;
  --green: #34d399;
  --green-dim: #2a9d6e;
  --green-bg: rgba(52, 211, 153, 0.1);
  --green-border: rgba(52, 211, 153, 0.2);
  --yellow: #fbbf24;
  --yellow-bg: rgba(251, 191, 36, 0.1);
  --yellow-border: rgba(251, 191, 36, 0.2);
  --red: #f87171;
  --poly-color: #3b82f6;
  --poly-bg: rgba(59, 130, 246, 0.08);
  --poly-border: rgba(59, 130, 246, 0.2);
  --kalshi-color: #a78bfa;
  --kalshi-bg: rgba(167, 139, 250, 0.08);
  --kalshi-border: rgba(167, 139, 250, 0.2);
  --radius: 10px;
  --radius-sm: 6px;
  --radius-xs: 4px;
}
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: 'Inter', -apple-system, BlinkMacSystemFont, sans-serif; background: var(--bg-primary); color: var(--text-primary); line-height: 1.4; -webkit-font-smoothing: antialiased; font-size: 13px; }

/* Header */
.header { position: sticky; top: 0; z-index: 50; background: rgba(6, 8, 13, 0.9); backdrop-filter: blur(20px) saturate(180%); border-bottom: 1px solid var(--border); }
.header-inner { max-width: 1200px; margin: 0 auto; padding: 10px 20px; display: flex; align-items: center; gap: 16px; }
.logo { display: flex; align-items: center; gap: 8px; text-decoration: none; flex-shrink: 0; }
.logo-icon { width: 28px; height: 28px; border-radius: 7px; background: linear-gradient(135deg, var(--accent), #6366f1); display: flex; align-items: center; justify-content: center; font-weight: 800; font-size: 13px; color: white; }
.logo-text { font-size: 1rem; font-weight: 700; color: var(--text-primary); letter-spacing: -0.3px; }
.search-form { display: flex; align-items: center; gap: 6px; flex: 1; max-width: 500px; margin-left: auto; }
.search-input { flex: 1; padding: 7px 14px; background: var(--bg-card); border: 1px solid var(--border); border-radius: 999px; color: var(--text-primary); font-size: 0.8rem; font-family: inherit; transition: all 200ms; }
.search-input:focus { outline: none; border-color: var(--accent); box-shadow: 0 0 0 2px var(--accent-glow); }
.search-input::placeholder { color: var(--text-muted); }
.search-btn { padding: 7px 16px; background: linear-gradient(135deg, #6366f1, #818cf8); border: none; border-radius: 999px; color: white; font-size: 0.8rem; font-weight: 600; font-family: inherit; cursor: pointer; transition: all 200ms; white-space: nowrap; }
.search-btn:hover { transform: translateY(-1px); box-shadow: 0 4px 16px rgba(99, 102, 241, 0.3); }

/* Main content */
.content { max-width: 1200px; margin: 0 auto; padding: 20px 20px; }

/* Hero */
.hero { display: flex; flex-direction: column; align-items: center; justify-content: center; min-height: 60vh; text-align: center; padding: 40px 20px; }
.hero-title { font-size: 2.2rem; font-weight: 800; letter-spacing: -1px; background: linear-gradient(135deg, var(--text-primary), var(--accent-bright)); -webkit-background-clip: text; -webkit-text-fill-color: transparent; background-clip: text; margin-bottom: 10px; }
.hero-sub { font-size: 0.9rem; color: var(--text-muted); max-width: 400px; margin-bottom: 28px; line-height: 1.5; }
.hero-form { display: flex; gap: 8px; width: 100%; max-width: 480px; }
.hero-input { flex: 1; padding: 12px 18px; background: var(--bg-card); border: 1px solid var(--border); border-radius: 999px; color: var(--text-primary); font-size: 0.9rem; font-family: inherit; transition: all 200ms; }
.hero-input:focus { outline: none; border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-glow); }
.hero-input::placeholder { color: var(--text-muted); }
.hero-btn { padding: 12px 24px; background: linear-gradient(135deg, #6366f1, #818cf8); border: none; border-radius: 999px; color: white; font-size: 0.9rem; font-weight: 600; font-family: inherit; cursor: pointer; transition: all 200ms; }
.hero-btn:hover { transform: translateY(-1px); box-shadow: 0 4px 24px rgba(99, 102, 241, 0.35); }
.hero-hints { display: flex; gap: 6px; flex-wrap: wrap; justify-content: center; margin-top: 16px; }
.hero-hint { padding: 5px 12px; background: var(--bg-card); border: 1px solid var(--border); border-radius: 999px; font-size: 0.75rem; color: var(--text-secondary); cursor: pointer; transition: all 150ms; text-decoration: none; }
.hero-hint:hover { border-color: var(--accent); color: var(--accent-bright); background: var(--accent-glow); }

/* Results header */
.results-header { display: flex; align-items: center; gap: 10px; margin-bottom: 16px; flex-wrap: wrap; }
.results-title { font-size: 0.85rem; font-weight: 600; color: var(--text-secondary); }
.results-title strong { color: var(--text-primary); }
.result-badge { display: inline-flex; align-items: center; gap: 3px; padding: 3px 8px; border-radius: 999px; font-size: 0.68rem; font-weight: 600; }
.badge-match { background: var(--green-bg); color: var(--green); border: 1px solid var(--green-border); }
.badge-probable { background: var(--yellow-bg); color: var(--yellow); border: 1px solid var(--yellow-border); }
.badge-venue { background: var(--bg-elevated); color: var(--text-secondary); border: 1px solid var(--border); }

/* Empty state */
.empty-state { text-align: center; padding: 60px 20px; }
.empty-icon { font-size: 2.5rem; color: var(--text-muted); margin-bottom: 12px; }
.empty-title { font-size: 1.1rem; font-weight: 600; margin-bottom: 6px; }
.empty-sub { font-size: 0.82rem; color: var(--text-muted); max-width: 420px; margin: 0 auto; }
.diagnosis { margin-top: 20px; text-align: left; max-width: 640px; margin-left: auto; margin-right: auto; }
.diagnosis-box { background: var(--bg-card); border: 1px solid var(--border); border-radius: var(--radius); padding: 14px; margin-bottom: 12px; }
.diagnosis-label { font-size: 0.68rem; font-weight: 700; letter-spacing: 0.05em; text-transform: uppercase; color: var(--text-muted); margin-bottom: 6px; }
.diagnosis-msg { font-size: 0.82rem; color: var(--text-secondary); line-height: 1.45; }

/* ── Compact pair card ────────────────────────────────────────────── */
.pair-card { background: var(--bg-card); border: 1px solid var(--border); border-radius: var(--radius); margin-bottom: 10px; overflow: hidden; transition: border-color 150ms, box-shadow 150ms; }
.pair-card:hover { border-color: var(--border-hover); box-shadow: 0 4px 24px rgba(0, 0, 0, 0.15); }
.pair-card.card-match { border-left: 3px solid var(--green); }
.pair-card.card-probable { border-left: 3px solid var(--yellow); }

/* Header row: index + badge + scores + route */
.pair-head { display: flex; align-items: center; gap: 8px; padding: 8px 12px; border-bottom: 1px solid var(--border); background: var(--bg-secondary); }
.pair-index { min-width: 20px; height: 20px; border-radius: var(--radius-xs); background: var(--bg-elevated); display: flex; align-items: center; justify-content: center; font-size: 0.68rem; font-weight: 700; color: var(--text-muted); flex-shrink: 0; }
.conf-badge { display: inline-flex; align-items: center; gap: 3px; padding: 2px 8px; border-radius: 999px; font-size: 0.68rem; font-weight: 600; letter-spacing: 0.02em; flex-shrink: 0; }
.conf-badge .material-icons-round { font-size: 12px; }
.conf-match { background: var(--green-bg); color: var(--green); border: 1px solid var(--green-border); }
.conf-probable { background: var(--yellow-bg); color: var(--yellow); border: 1px solid var(--yellow-border); }
.conf-no { background: rgba(248, 113, 113, 0.1); color: var(--red); border: 1px solid rgba(248, 113, 113, 0.2); }
.head-separator { width: 1px; height: 16px; background: var(--border); flex-shrink: 0; }
.score-pills { display: flex; gap: 6px; flex-wrap: wrap; }
.score-pill { display: flex; align-items: center; gap: 4px; font-size: 0.67rem; color: var(--text-muted); }
.score-pill strong { color: var(--text-secondary); font-weight: 600; }
.score-pill .bar-mini { width: 24px; height: 3px; background: rgba(255,255,255,0.06); border-radius: 2px; overflow: hidden; }
.score-pill .bar-mini-fill { height: 100%; background: var(--accent); border-radius: 2px; }
.head-spacer { flex: 1; }
.route-chip { display: inline-flex; align-items: center; gap: 4px; font-size: 0.68rem; color: var(--text-muted); flex-shrink: 0; }
.route-chip .material-icons-round { font-size: 13px; color: var(--green-dim); }
.route-chip .rv { padding: 1px 8px; border-radius: 999px; font-weight: 600; font-size: 0.67rem; }
.route-chip .rv.venue-poly { background: var(--poly-bg); color: var(--poly-color); border: 1px solid var(--poly-border); }
.route-chip .rv.venue-kalshi { background: var(--kalshi-bg); color: var(--kalshi-color); border: 1px solid var(--kalshi-border); }
.expand-btn { background: none; border: none; color: var(--text-muted); cursor: pointer; font-size: 0.68rem; font-family: inherit; display: flex; align-items: center; gap: 2px; padding: 2px 4px; border-radius: var(--radius-xs); transition: all 150ms; flex-shrink: 0; }
.expand-btn:hover { color: var(--accent); background: var(--accent-glow); }
.expand-btn .material-icons-round { font-size: 14px; transition: transform 200ms; }
.expand-btn.is-open .material-icons-round { transform: rotate(180deg); }

/* Body: two markets side by side, compact */
.pair-body { display: grid; grid-template-columns: 1fr 1fr; }
.market-col { padding: 10px 12px; cursor: pointer; transition: background 150ms; }
.market-col:first-child { border-right: 1px solid var(--border); }
.market-col:hover { background: var(--bg-card-hover); }

/* Market header: venue chip + title on one line */
.mkt-header { display: flex; align-items: flex-start; gap: 8px; margin-bottom: 6px; }
.mkt-thumb { width: 36px; height: 36px; border-radius: 6px; object-fit: cover; flex-shrink: 0; background: var(--bg-elevated); }
.venue-dot { width: 18px; height: 18px; border-radius: 50%; display: flex; align-items: center; justify-content: center; font-size: 0.6rem; font-weight: 800; color: white; flex-shrink: 0; margin-top: 1px; }
.venue-dot.vd-polymarket { background: var(--poly-color); }
.venue-dot.vd-kalshi { background: var(--kalshi-color); }
.mkt-title { font-size: 0.82rem; font-weight: 600; color: var(--text-primary); line-height: 1.35; }

/* Price + stats row */
.mkt-stats { display: flex; align-items: baseline; gap: 10px; flex-wrap: wrap; }
.mkt-price { font-size: 1.1rem; font-weight: 800; letter-spacing: -0.5px; background: linear-gradient(135deg, var(--green), #6ee7b7); -webkit-background-clip: text; -webkit-text-fill-color: transparent; background-clip: text; }
.mkt-stat { font-size: 0.7rem; color: var(--text-muted); }
.mkt-stat span { color: var(--text-secondary); font-weight: 500; }

/* Expand area */
.pair-explain { max-height: 0; overflow: hidden; transition: max-height 300ms ease; }
.pair-explain.is-open { max-height: 500px; overflow-y: auto; }
.pair-explain-inner { padding: 10px 12px; border-top: 1px solid var(--border); font-size: 0.73rem; color: var(--text-secondary); line-height: 1.5; white-space: pre-wrap; background: rgba(0,0,0,0.15); }
.pair-explain-section { margin-bottom: 6px; }
.pair-explain-label { font-size: 0.65rem; font-weight: 600; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.04em; margin-bottom: 2px; }

/* Modal */
.modal-overlay { position: fixed; inset: 0; display: none; align-items: center; justify-content: center; z-index: 100; }
.modal-overlay.is-open { display: flex; }
.modal-bg { position: absolute; inset: 0; background: rgba(0, 0, 0, 0.7); backdrop-filter: blur(8px); }
.modal-container { position: relative; width: min(880px, 94vw); max-height: 90vh; background: var(--bg-card); border: 1px solid var(--border-hover); border-radius: var(--radius); z-index: 1; box-shadow: 0 25px 80px rgba(0, 0, 0, 0.5); display: flex; flex-direction: column; animation: modalIn 200ms ease; }
@keyframes modalIn { from { opacity: 0; transform: translateY(10px) scale(0.98); } to { opacity: 1; transform: none; } }
.modal-header { padding: 14px 18px; border-bottom: 1px solid var(--border); display: flex; align-items: center; justify-content: space-between; flex-shrink: 0; }
.modal-header-title { font-size: 0.9rem; font-weight: 700; color: var(--text-primary); }
.modal-close-btn { width: 28px; height: 28px; border-radius: 6px; background: var(--bg-elevated); border: 1px solid var(--border); color: var(--text-secondary); display: flex; align-items: center; justify-content: center; cursor: pointer; transition: all 150ms; }
.modal-close-btn:hover { background: rgba(248, 113, 113, 0.1); color: var(--red); border-color: rgba(248, 113, 113, 0.3); }
.modal-close-btn .material-icons-round { font-size: 16px; }
.modal-scroll { padding: 16px 18px; overflow-y: auto; flex: 1; }
.modal-section { margin-bottom: 16px; }
.modal-section-title { font-size: 0.68rem; font-weight: 600; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.05em; margin-bottom: 8px; padding-bottom: 4px; border-bottom: 1px solid var(--border); }
.modal-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
.modal-field { padding: 8px 10px; background: var(--bg-primary); border-radius: var(--radius-xs); border: 1px solid rgba(255,255,255,0.03); }
.modal-field-label { font-size: 0.63rem; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.04em; margin-bottom: 2px; }
.modal-field-value { font-size: 0.8rem; color: var(--text-primary); font-weight: 500; word-break: break-word; }
.modal-field.full { grid-column: 1 / -1; }
.raw-json { border: 1px solid var(--border); border-radius: var(--radius-sm); padding: 10px; background: var(--bg-primary); max-height: 220px; overflow: auto; white-space: pre-wrap; font-family: 'SF Mono', 'Fira Code', monospace; font-size: 0.68rem; color: var(--text-secondary); line-height: 1.5; }
.modal-links { display: flex; gap: 6px; flex-wrap: wrap; margin-top: 10px; }
.modal-link { display: inline-flex; align-items: center; gap: 4px; padding: 6px 12px; background: var(--bg-elevated); border: 1px solid var(--border); border-radius: var(--radius-xs); color: var(--accent-bright); font-size: 0.75rem; font-weight: 500; text-decoration: none; transition: all 150ms; }
.modal-link:hover { border-color: var(--accent); background: var(--accent-glow); }
.modal-link .material-icons-round { font-size: 14px; }
.live-panel { border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--bg-primary); padding: 10px; }
.live-head { display: flex; align-items: baseline; justify-content: space-between; margin-bottom: 8px; }
.live-now { font-size: 1.05rem; font-weight: 800; letter-spacing: -0.4px; color: var(--text-primary); }
.live-delta { font-size: 0.78rem; font-weight: 700; }
.live-delta.up { color: var(--green); }
.live-delta.down { color: var(--red); }
.live-delta.flat { color: var(--text-muted); }

@media (max-width: 768px) {
  .pair-body { grid-template-columns: 1fr; }
  .market-col:first-child { border-right: none; border-bottom: 1px solid var(--border); }
  .header-inner { padding: 8px 12px; }
  .content { padding: 12px; }
  .modal-grid { grid-template-columns: 1fr; }
  .score-pills { display: none; }
  .hero-title { font-size: 1.8rem; }
}

/* ── Search loader ───────────────────────────────────────────── */
.search-loader { max-width: 500px; margin: 72px auto 0; }
.loader-query { font-size: 1rem; font-weight: 600; color: var(--text-primary); margin-bottom: 28px; }
.loader-query em { color: var(--accent-bright); font-style: normal; }
.loader-log { display: flex; flex-direction: column; gap: 0; }
.loader-line {
  display: flex; align-items: center; gap: 10px;
  font-size: 0.84rem; padding: 7px 0;
  border-bottom: 1px solid rgba(255,255,255,0.04);
  opacity: 0; animation: loaderIn 280ms ease forwards;
}
.loader-line:last-child { border-bottom: none; }
.loader-icon { width: 22px; height: 22px; display: flex; align-items: center; justify-content: center; flex-shrink: 0; font-size: 15px; border-radius: 50%; }
.loader-icon.spin { animation: loaderSpin 0.9s linear infinite; color: var(--accent-bright); }
.loader-icon.ok { color: #4ade80; }
.loader-icon.warn { color: #f59e0b; }
.loader-msg { flex: 1; line-height: 1.35; }
.loader-step .loader-msg { color: var(--text-muted); }
.loader-result .loader-msg { color: var(--text-secondary); }
.loader-count { font-size: 0.75rem; font-weight: 700; padding: 1px 8px; border-radius: 999px; background: var(--bg-elevated); color: var(--accent-bright); margin-left: auto; flex-shrink: 0; }
.loader-venue-poly { color: #818cf8; font-size: 0.68rem; font-weight: 700; text-transform: uppercase; letter-spacing: 0.04em; margin-left: auto; }
.loader-venue-kalshi { color: #fbbf24; font-size: 0.68rem; font-weight: 700; text-transform: uppercase; letter-spacing: 0.04em; margin-left: auto; }
@keyframes loaderIn { from { opacity:0; transform:translateX(-10px); } to { opacity:1; transform:translateX(0); } }
@keyframes loaderSpin { to { transform: rotate(360deg); } }

</style>
</head>
<body>

<div class="header">
  <div class="header-inner">
    <a href="/" class="logo">
      <div class="logo-icon">E</div>
      <div class="logo-text">Equinox</div>
    </a>
    {{if .HasQuery}}
    <form class="search-form" method="GET" action="/">
      <input class="search-input" type="text" name="q" value="{{.SearchQuery}}" placeholder="Search markets across venues..." autofocus>
      <button class="search-btn" type="submit">Search</button>
    </form>
    {{end}}
  </div>
</div>

<div class="content">

{{if not .HasQuery}}
<!-- Hero / landing -->
<div class="hero">
  <div class="hero-title">Cross-venue market search</div>
  <div class="hero-sub">Find equivalent prediction markets on Polymarket and Kalshi side by side, with routing recommendations.</div>
  <form class="hero-form" method="GET" action="/">
    <input class="hero-input" type="text" name="q" placeholder="e.g. 2026 FIFA World Cup, Bitcoin, Fed rate..." autofocus>
    <button class="hero-btn" type="submit">Search</button>
  </form>
  <div class="hero-hints">
    <a class="hero-hint" href="/?q=2026+FIFA+World+Cup">2026 FIFA World Cup</a>
    <a class="hero-hint" href="/?q=2028+US+Presidential+Election">2028 US Presidential Election</a>
    <a class="hero-hint" href="/?q=Oscars+2026">Oscars 2026</a>
    <a class="hero-hint" href="/?q=Bitcoin+price">Bitcoin price</a>
    <a class="hero-hint" href="/?q=NBA+championship+2026">NBA championship 2026</a>
  </div>
</div>

{{else if not .Pairs}}
<!-- No results -->
<div class="empty-state">
  <div class="empty-icon material-icons-round">search_off</div>
  <div class="empty-title">No equivalent pairs found</div>
  {{if .DiagnosisMessage}}
  <div class="diagnosis">
    <div class="diagnosis-box">
      <div class="diagnosis-label">Why no matches?</div>
      <div class="diagnosis-msg">{{.DiagnosisMessage}}</div>
    </div>
  </div>
  {{else}}
  <div class="empty-sub">Try a different search query, or adjust MATCH_THRESHOLD / MAX_DATE_DELTA_DAYS to widen the match window.</div>
  {{end}}
</div>

{{else}}
<!-- Results -->
<div class="results-header">
  <div class="results-title">Results for <strong>"{{.SearchQuery}}"</strong></div>
  {{range $venue, $count := .VenueCounts}}
  <span class="result-badge badge-venue">{{$venue}}: {{$count}}</span>
  {{end}}
  {{if .MatchCount}}<span class="result-badge badge-match">{{.MatchCount}} matched</span>{{end}}
  {{if .ProbableCount}}<span class="result-badge badge-probable">{{.ProbableCount}} probable</span>{{end}}
</div>

{{range $i, $p := .Pairs}}
<div class="pair-card {{cardClass $p.Confidence}}" id="pair-{{$i}}">
  <div class="pair-head">
    <div class="pair-index">{{inc $i}}</div>
    <div class="conf-badge {{confClass $p.Confidence}}">
      <span class="material-icons-round">{{confIcon $p.Confidence}}</span>
      {{$p.Confidence}}
    </div>
    <div class="head-separator"></div>
    <div class="score-pills">
      <div class="score-pill">Fuzzy <div class="bar-mini"><div class="bar-mini-fill" style="width:{{scoreWidth $p.FuzzyScore}}"></div></div> <strong>{{score $p.FuzzyScore}}</strong></div>
      <div class="score-pill">Embed <div class="bar-mini"><div class="bar-mini-fill" style="width:{{scoreWidth $p.EmbeddingScore}}"></div></div> <strong>{{score $p.EmbeddingScore}}</strong></div>
      <div class="score-pill">Composite <div class="bar-mini"><div class="bar-mini-fill" style="width:{{scoreWidth $p.CompositeScore}}"></div></div> <strong>{{score $p.CompositeScore}}</strong></div>
    </div>
    <div class="head-spacer"></div>
    <div class="route-chip">
      <span class="material-icons-round">arrow_forward</span>
      <span class="rv {{venueClass $p.SelectedVenue}}">{{$p.SelectedVenue}}</span>
    </div>
    <button class="expand-btn" onclick="toggleExplain(this, 'explain-{{$i}}')">
      <span class="material-icons-round">expand_more</span>
    </button>
  </div>
  <div class="pair-body">
    <div class="market-col clickable-market"
         data-venue="{{$p.MarketA.Venue}}" data-market-id="{{$p.MarketA.VenueMarketID}}" data-title="{{$p.MarketA.Title}}"
         data-description="{{$p.MarketA.Description}}" data-category="{{$p.MarketA.Category}}" data-tags="{{$p.MarketA.Tags}}"
         data-status="{{$p.MarketA.Status}}" data-yes="{{printf "%.6f" $p.MarketA.YesPrice}}"
         data-token-id="{{$p.MarketA.VenueYesTokenID}}"
         data-liquidity="{{printf "%.2f" $p.MarketA.Liquidity}}" data-spread="{{printf "%.6f" $p.MarketA.Spread}}"
         data-resolution-date="{{$p.MarketA.ResolutionDate}}" data-created-at="{{$p.MarketA.CreatedAt}}" data-updated-at="{{$p.MarketA.UpdatedAt}}"
         data-volume24h="{{printf "%.2f" $p.MarketA.Volume24h}}" data-open-interest="{{printf "%.2f" $p.MarketA.OpenInterest}}"
         data-resolution-criteria="{{$p.MarketA.ResolutionRaw}}" data-venue-link="{{$p.MarketA.VenueLink}}"
         data-venue-search-link="{{$p.MarketA.VenueSearchLink}}" data-venue-search-link-alt="{{$p.MarketA.VenueSearchLinkAlt}}"
         data-image-url="{{$p.MarketA.ImageURL}}"
         data-payload="{{$p.MarketA.RawPayloadB64}}">
      <div class="mkt-header">
        {{if $p.MarketA.ImageURL}}<img class="mkt-thumb" src="{{$p.MarketA.ImageURL}}" alt="" loading="lazy">{{end}}
        <div class="venue-dot vd-{{$p.MarketA.Venue}}">{{venueIcon $p.MarketA.Venue}}</div>
        <div class="mkt-title">{{$p.MarketA.Title}}</div>
      </div>
      <div class="mkt-stats">
        <span class="mkt-price" data-venue="{{$p.MarketA.Venue}}" data-market-id="{{$p.MarketA.VenueMarketID}}" data-token-id="{{$p.MarketA.VenueYesTokenID}}">{{pct $p.MarketA.YesPrice}}</span>
        <span class="mkt-stat">Liq <span>{{usd $p.MarketA.Liquidity}}</span></span>
        <span class="mkt-stat">Spread <span>{{if $p.MarketA.Spread}}{{pct $p.MarketA.Spread}}{{else}}--{{end}}</span></span>
        {{if $p.MarketA.ResolutionDate}}<span class="mkt-stat">Res <span>{{$p.MarketA.ResolutionDate}}</span></span>{{end}}
        {{if bigVol $p.MarketA.Volume24h}}<span class="mkt-stat">24h <span>{{usd $p.MarketA.Volume24h}}</span></span>{{end}}
      </div>
    </div>
    <div class="market-col clickable-market"
         data-venue="{{$p.MarketB.Venue}}" data-market-id="{{$p.MarketB.VenueMarketID}}" data-title="{{$p.MarketB.Title}}"
         data-description="{{$p.MarketB.Description}}" data-category="{{$p.MarketB.Category}}" data-tags="{{$p.MarketB.Tags}}"
         data-status="{{$p.MarketB.Status}}" data-yes="{{printf "%.6f" $p.MarketB.YesPrice}}"
         data-token-id="{{$p.MarketB.VenueYesTokenID}}"
         data-liquidity="{{printf "%.2f" $p.MarketB.Liquidity}}" data-spread="{{printf "%.6f" $p.MarketB.Spread}}"
         data-resolution-date="{{$p.MarketB.ResolutionDate}}" data-created-at="{{$p.MarketB.CreatedAt}}" data-updated-at="{{$p.MarketB.UpdatedAt}}"
         data-volume24h="{{printf "%.2f" $p.MarketB.Volume24h}}" data-open-interest="{{printf "%.2f" $p.MarketB.OpenInterest}}"
         data-resolution-criteria="{{$p.MarketB.ResolutionRaw}}" data-venue-link="{{$p.MarketB.VenueLink}}"
         data-venue-search-link="{{$p.MarketB.VenueSearchLink}}" data-venue-search-link-alt="{{$p.MarketB.VenueSearchLinkAlt}}"
         data-image-url="{{$p.MarketB.ImageURL}}"
         data-payload="{{$p.MarketB.RawPayloadB64}}">
      <div class="mkt-header">
        {{if $p.MarketB.ImageURL}}<img class="mkt-thumb" src="{{$p.MarketB.ImageURL}}" alt="" loading="lazy">{{end}}
        <div class="venue-dot vd-{{$p.MarketB.Venue}}">{{venueIcon $p.MarketB.Venue}}</div>
        <div class="mkt-title">{{$p.MarketB.Title}}</div>
      </div>
      <div class="mkt-stats">
        <span class="mkt-price" data-venue="{{$p.MarketB.Venue}}" data-market-id="{{$p.MarketB.VenueMarketID}}" data-token-id="{{$p.MarketB.VenueYesTokenID}}">{{pct $p.MarketB.YesPrice}}</span>
        <span class="mkt-stat">Liq <span>{{usd $p.MarketB.Liquidity}}</span></span>
        <span class="mkt-stat">Spread <span>{{if $p.MarketB.Spread}}{{pct $p.MarketB.Spread}}{{else}}--{{end}}</span></span>
        {{if $p.MarketB.ResolutionDate}}<span class="mkt-stat">Res <span>{{$p.MarketB.ResolutionDate}}</span></span>{{end}}
        {{if bigVol $p.MarketB.Volume24h}}<span class="mkt-stat">24h <span>{{usd $p.MarketB.Volume24h}}</span></span>{{end}}
      </div>
    </div>
  </div>
  <div class="pair-explain" id="explain-{{$i}}">
    <div class="pair-explain-inner">
      <div class="pair-explain-section"><div class="pair-explain-label">Match reasoning</div>{{$p.Explanation}}</div>
      <div class="pair-explain-section"><div class="pair-explain-label">Routing decision</div>{{$p.RoutingReason}}</div>
    </div>
  </div>
</div>
{{end}}
{{end}}

</div>

<!-- Detail Modal -->
<div id="marketDetailModal" class="modal-overlay" aria-hidden="true">
  <div class="modal-bg" onclick="closeMarketModal()"></div>
  <div class="modal-container">
    <div id="mdImageBanner" style="display:none;height:120px;overflow:hidden;border-radius:var(--radius) var(--radius) 0 0;">
      <img id="mdImage" src="" alt="" style="width:100%;height:100%;object-fit:cover;">
    </div>
    <div class="modal-header">
      <div class="modal-header-title" id="mdTitle"></div>
      <button class="modal-close-btn" onclick="closeMarketModal()">
        <span class="material-icons-round">close</span>
      </button>
    </div>
    <div class="modal-scroll">
      <div class="modal-section">
        <div class="modal-section-title">Market Details</div>
        <div class="modal-grid">
          <div class="modal-field"><div class="modal-field-label">Venue</div><div class="modal-field-value" id="mdVenue"></div></div>
          <div class="modal-field"><div class="modal-field-label">Market ID</div><div class="modal-field-value" id="mdMarketId"></div></div>
          <div class="modal-field"><div class="modal-field-label">Status</div><div class="modal-field-value" id="mdStatus"></div></div>
          <div class="modal-field"><div class="modal-field-label">Category</div><div class="modal-field-value" id="mdCategory"></div></div>
          <div class="modal-field full"><div class="modal-field-label">Description</div><div class="modal-field-value" id="mdDescription"></div></div>
          <div class="modal-field full"><div class="modal-field-label">Tags</div><div class="modal-field-value" id="mdTags"></div></div>
        </div>
      </div>
      <div class="modal-section">
        <div class="modal-section-title">Pricing & Liquidity</div>
        <div class="modal-grid">
          <div class="modal-field"><div class="modal-field-label">Yes Price</div><div class="modal-field-value" id="mdYes"></div></div>
          <div class="modal-field"><div class="modal-field-label">Spread</div><div class="modal-field-value" id="mdSpread"></div></div>
          <div class="modal-field"><div class="modal-field-label">Liquidity</div><div class="modal-field-value" id="mdLiquidity"></div></div>
          <div class="modal-field"><div class="modal-field-label">24h Volume</div><div class="modal-field-value" id="mdVolume"></div></div>
          <div class="modal-field"><div class="modal-field-label">Open Interest</div><div class="modal-field-value" id="mdOpenInterest"></div></div>
          <div class="modal-field"><div class="modal-field-label">Resolution Date</div><div class="modal-field-value" id="mdResolutionDate"></div></div>
        </div>
      </div>
      <div class="modal-section" id="mdLiveSection" style="display:none;">
        <div class="modal-section-title">Live Price</div>
        <div class="live-panel">
          <div class="live-head">
            <div class="live-now" id="mdLiveNow">--</div>
            <div class="live-delta flat" id="mdLiveDelta">--</div>
          </div>
        </div>
      </div>
      <div class="modal-section">
        <div class="modal-section-title">Resolution</div>
        <div class="modal-grid">
          <div class="modal-field full"><div class="modal-field-label">Resolution Criteria</div><div class="modal-field-value" id="mdResolutionCriteria"></div></div>
        </div>
      </div>
      <div class="modal-section">
        <div class="modal-section-title">Raw Venue Payload</div>
        <pre class="raw-json" id="mdRawPayload"></pre>
      </div>
      <div class="modal-links" id="mdLinks"></div>
    </div>
  </div>
</div>

<script>
(function() {
  if (window.__livePriceState) return;
  var historyByKey = {};
  var latestByKey = {};
  var maxPoints = 120;

  function mkKey(venue, marketId) {
    return String((venue || "").toLowerCase()) + ":" + String(marketId || "");
  }

  function publish(venue, marketId, yesProb) {
    if (typeof yesProb !== "number" || !isFinite(yesProb) || !marketId) return;
    if (yesProb < 0) yesProb = 0;
    if (yesProb > 1) yesProb = 1;
    var key = mkKey(venue, marketId);
    if (!historyByKey[key]) historyByKey[key] = [];
    historyByKey[key].push({ t: Date.now(), p: yesProb });
    if (historyByKey[key].length > maxPoints) historyByKey[key].shift();
    latestByKey[key] = yesProb;
    window.dispatchEvent(new CustomEvent("equinox-price-tick", { detail: { key: key, p: yesProb } }));
  }

  window.__livePriceState = {
    key: mkKey,
    publish: publish,
    latest: function(venue, marketId) { return latestByKey[mkKey(venue, marketId)]; },
    history: function(venue, marketId) { return (historyByKey[mkKey(venue, marketId)] || []).slice(); }
  };
})();

(function() {
  window.toggleExplain = function(btn, id) {
    var el = document.getElementById(id);
    if (!el) return;
    var open = el.classList.toggle("is-open");
    btn.classList.toggle("is-open", open);
  };

  var modal = document.getElementById("marketDetailModal");
  if (!modal) return;

  var fields = {};
  ["mdTitle","mdVenue","mdMarketId","mdStatus","mdDescription","mdTags",
   "mdCategory","mdResolutionDate","mdResolutionCriteria","mdYes",
   "mdLiquidity","mdSpread","mdVolume",
   "mdOpenInterest","mdRawPayload","mdLinks","mdImage","mdImageBanner",
   "mdLiveNow","mdLiveDelta","mdLiveSection"].forEach(function(id) {
    fields[id] = document.getElementById(id);
  });

  function safe(v) { return v ? String(v) : "--"; }
  var activeLive = { venue: "", marketId: "" };

  function renderLivePrice(venue, marketId) {
    var st = window.__livePriceState;
    if (!st) { fields.mdLiveSection.style.display = "none"; return; }
    var latest = st.latest(venue, marketId);
    if (latest == null) { fields.mdLiveSection.style.display = "none"; return; }
    fields.mdLiveSection.style.display = "";
    fields.mdLiveNow.textContent = (latest * 100).toFixed(1) + "%";

    var hist = st.history(venue, marketId);
    if (hist.length >= 2) {
      var first = hist[0].p;
      var diff = latest - first;
      var cls = "flat";
      if (diff > 0.0001) cls = "up";
      else if (diff < -0.0001) cls = "down";
      var sign = diff > 0 ? "+" : "";
      fields.mdLiveDelta.textContent = sign + (diff * 100).toFixed(2) + "%";
      fields.mdLiveDelta.className = "live-delta " + cls;
    } else {
      fields.mdLiveDelta.textContent = "live";
      fields.mdLiveDelta.className = "live-delta flat";
    }
  }

  window.showMarketModal = function(card) {
    var d = card.dataset;
    fields.mdTitle.textContent = safe(d.title);
    fields.mdVenue.textContent = safe(d.venue);
    fields.mdMarketId.textContent = safe(d.marketId);
    fields.mdStatus.textContent = safe(d.status);
    fields.mdDescription.textContent = safe(d.description);
    fields.mdTags.textContent = safe(d.tags);
    fields.mdCategory.textContent = safe(d.category);
    fields.mdResolutionDate.textContent = safe(d.resolutionDate);
    fields.mdResolutionCriteria.textContent = safe(d.resolutionCriteria);
    fields.mdVolume.textContent = safe(d.volume24h);
    fields.mdOpenInterest.textContent = safe(d.openInterest);
    fields.mdYes.textContent = safe(d.yes);
    fields.mdLiquidity.textContent = safe(d.liquidity);
    fields.mdSpread.textContent = safe(d.spread);

    activeLive.venue = String(d.venue || "").toLowerCase();
    activeLive.marketId = String(d.marketId || "");
    var initYes = parseFloat(d.yes);
    if (window.__livePriceState && isFinite(initYes)) {
      window.__livePriceState.publish(activeLive.venue, activeLive.marketId, initYes);
    }
    renderLivePrice(activeLive.venue, activeLive.marketId);

    var imgUrl = d.imageUrl || "";
    if (imgUrl) {
      fields.mdImage.src = imgUrl;
      fields.mdImageBanner.style.display = "block";
    } else {
      fields.mdImage.src = "";
      fields.mdImageBanner.style.display = "none";
    }

    var b64 = d.payload || "";
    if (b64) {
      try {
        fields.mdRawPayload.textContent = JSON.stringify(JSON.parse(atob(b64)), null, 2);
      } catch(e) { fields.mdRawPayload.textContent = b64; }
    } else {
      fields.mdRawPayload.textContent = "No raw payload available.";
    }

    var links = fields.mdLinks;
    links.innerHTML = "";
    var venueLink = safe(d.venueLink);
    if (venueLink && venueLink !== "--") {
      links.innerHTML += '<a class="modal-link" href="' + venueLink + '" target="_blank"><span class="material-icons-round">open_in_new</span>Open on ' + safe(d.venue) + '</a>';
    }

    modal.classList.add("is-open");
    modal.setAttribute("aria-hidden", "false");
    document.body.style.overflow = "hidden";
  }

  document.querySelectorAll(".clickable-market").forEach(function(card) {
    card.addEventListener("click", function() { window.showMarketModal(card); });
  });

  window.closeMarketModal = function() {
    modal.classList.remove("is-open");
    modal.setAttribute("aria-hidden", "true");
    document.body.style.overflow = "";
    activeLive.venue = "";
    activeLive.marketId = "";
  };

  window.addEventListener("keydown", function(e) {
    if (e.key === "Escape") window.closeMarketModal();
  });

  document.querySelectorAll(".pair-card").forEach(function(card, i) {
    card.style.opacity = "0";
    card.style.transform = "translateY(16px)";
    card.style.transition = "opacity 400ms ease " + (i * 60) + "ms, transform 400ms ease " + (i * 60) + "ms";
  });
  var observer = new IntersectionObserver(function(entries) {
    entries.forEach(function(entry) {
      if (entry.isIntersecting) {
        entry.target.style.opacity = "1";
        entry.target.style.transform = "translateY(0)";
      }
    });
  }, { threshold: 0.05 });
  document.querySelectorAll(".pair-card").forEach(function(card) { observer.observe(card); });

  window.addEventListener("equinox-price-tick", function() {
    if (!activeLive.venue || !activeLive.marketId) return;
    if (!modal.classList.contains("is-open")) return;
    renderLivePrice(activeLive.venue, activeLive.marketId);
  });
})();

/* ── Live Polymarket prices (WSS) ───────────────────────── */
(function() {
  var priceEls = Array.prototype.slice.call(document.querySelectorAll(".mkt-price[data-token-id]"));
  if (!priceEls.length) return;

  var byToken = {};
  priceEls.forEach(function(el) {
    var venue = (el.dataset.venue || "").toLowerCase();
    var token = (el.dataset.tokenId || "").trim();
    if (venue !== "polymarket" || !token) return;
    if (!byToken[token]) byToken[token] = [];
    byToken[token].push(el);
  });

  var tokenIDs = Object.keys(byToken);
  if (!tokenIDs.length) return;

  function renderProbability(token, p) {
    if (typeof p !== "number" || !isFinite(p)) return;
    if (p < 0) p = 0;
    if (p > 1) p = 1;
    var txt = (p * 100).toFixed(1) + "%";
    (byToken[token] || []).forEach(function(el) {
      el.textContent = txt;
      if (window.__livePriceState) {
        window.__livePriceState.publish("polymarket", el.dataset.marketId, p);
      }
    });
  }

  function extractProb(msg) {
    function toNum(v) {
      if (typeof v === "number" && isFinite(v)) return v;
      if (typeof v === "string" && v.trim() !== "") {
        var n = parseFloat(v);
        if (isFinite(n)) return n;
      }
      return null;
    }
    var p = null;
    var price = toNum(msg.price);
    var lastTrade = toNum(msg.last_trade_price);
    var bestBid = toNum(msg.best_bid);
    var bestAsk = toNum(msg.best_ask);
    if (price != null) p = price;
    else if (lastTrade != null) p = lastTrade;
    else if (bestBid != null && bestAsk != null) p = (bestBid + bestAsk) / 2;
    else if (bestBid != null) p = bestBid;
    else if (bestAsk != null) p = bestAsk;
    if (p == null) return null;
    // Some feeds send cents-style prices. Normalize if needed.
    if (p > 1.000001) p = p / 100.0;
    return p;
  }

  var ws = new WebSocket("wss://ws-subscriptions-clob.polymarket.com/ws/market");
  ws.onopen = function() {
    // Docs have used both asset_ids and assets_ids in examples.
    var payload = { type: "market", asset_ids: tokenIDs, assets_ids: tokenIDs };
    ws.send(JSON.stringify(payload));
  };
  ws.onmessage = function(evt) {
    var data;
    try { data = JSON.parse(evt.data); } catch (_) { return; }
    var msgs = Array.isArray(data) ? data : [data];
    msgs.forEach(function(msg) {
      if (!msg || typeof msg !== "object") return;
      var token = String(msg.asset_id || msg.assetId || "").trim();
      if (!token || !byToken[token]) return;
      var prob = extractProb(msg);
      if (prob == null) return;
      renderProbability(token, prob);
    });
  };
  ws.onerror = function() {};
})();

/* ── Live Kalshi prices (WSS ticker) ─────────────────────── */
(function() {
  var priceEls = Array.prototype.slice.call(document.querySelectorAll(".mkt-price[data-market-id]"));
  if (!priceEls.length) return;

  var byTicker = {};
  priceEls.forEach(function(el) {
    var venue = (el.dataset.venue || "").toLowerCase();
    var ticker = (el.dataset.marketId || "").trim();
    if (venue !== "kalshi" || !ticker) return;
    if (!byTicker[ticker]) byTicker[ticker] = [];
    byTicker[ticker].push(el);
  });

  var tickers = Object.keys(byTicker);
  if (!tickers.length) return;

  function renderTicker(ticker, yesProb) {
    if (typeof yesProb !== "number" || !isFinite(yesProb)) return;
    if (yesProb < 0) yesProb = 0;
    if (yesProb > 1) yesProb = 1;
    var txt = (yesProb * 100).toFixed(1) + "%";
    (byTicker[ticker] || []).forEach(function(el) {
      el.textContent = txt;
      if (window.__livePriceState) {
        window.__livePriceState.publish("kalshi", ticker, yesProb);
      }
    });
  }

  var ws = new WebSocket("wss://api.elections.kalshi.com/trade-api/ws/v2");
  ws.onopen = function() {
    ws.send(JSON.stringify({
      id: 1,
      cmd: "subscribe",
      params: {
        channels: ["ticker"],
        market_tickers: tickers
      }
    }));
  };
  ws.onmessage = function(evt) {
    var data;
    try { data = JSON.parse(evt.data); } catch (_) { return; }
    if (!data || data.type !== "ticker" || !data.msg) return;

    var ticker = String(data.msg.market_ticker || "").trim();
    if (!ticker || !byTicker[ticker]) return;

    var yesBid = data.msg.yes_bid;
    var yesAsk = data.msg.yes_ask;
    if (typeof yesBid === "string" && yesBid.trim() !== "") yesBid = parseFloat(yesBid);
    if (typeof yesAsk === "string" && yesAsk.trim() !== "") yesAsk = parseFloat(yesAsk);
    var yes = null;
    if (typeof yesBid === "number" && typeof yesAsk === "number") yes = (yesBid + yesAsk) / 2 / 100.0;
    else if (typeof yesBid === "number") yes = yesBid / 100.0;
    else if (typeof yesAsk === "number") yes = yesAsk / 100.0;
    if (yes == null) return;
    renderTicker(ticker, yes);
  };
  ws.onerror = function() {};
})();

/* ── SSE search loader with incremental pair streaming ──── */
(function() {
  var content = document.querySelector(".content");

  function showStreamUI(q) {
    content.innerHTML =
      '<div class="results-header" id="streamHeader">' +
        '<div class="results-title"><span class="material-icons-round" style="font-size:14px;vertical-align:middle;animation:loaderSpin 0.9s linear infinite;margin-right:4px;">sync</span> Searching for <strong>"' + escHtml(q) + '"</strong></div>' +
      '</div>' +
      '<div id="streamPairs"></div>' +
      '<div class="search-loader" id="streamLoader" style="margin-top:16px;">' +
        '<div class="loader-log" id="loaderLog"></div>' +
      '</div>';
  }

  function addLine(cls, iconCls, iconChar, msg, extra) {
    var log = document.getElementById("loaderLog");
    if (!log) return;
    var delay = log.children.length * 60;
    var line = document.createElement("div");
    line.className = "loader-line " + cls;
    line.style.animationDelay = delay + "ms";
    line.innerHTML =
      '<span class="loader-icon ' + iconCls + '">' + iconChar + '</span>' +
      '<span class="loader-msg">' + escHtml(msg) + '</span>' +
      (extra || "");
    log.appendChild(line);
  }

  function escHtml(s) {
    return String(s).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;");
  }

  function venueTag(venue) {
    if (!venue) return "";
    return '<span class="loader-venue-' + venue + '">' + venue + '</span>';
  }

  function countBadge(n) {
    if (!n) return "";
    return '<span class="loader-count">' + n + '</span>';
  }

  function fmtPct(f) { return (f * 100).toFixed(1) + "%"; }
  function fmtScore(f) { return f.toFixed(3); }
  function fmtScoreWidth(f) { return (f * 100).toFixed(1) + "%"; }
  function fmtUsd(f) {
    if (!f) return "--";
    if (f >= 1000000) return "$" + (f / 1000000).toFixed(1) + "M";
    if (f >= 1000) return "$" + (f / 1000).toFixed(1) + "K";
    return "$" + Math.round(f);
  }
  function confClass(c) { return c === "MATCH" ? "conf-match" : c === "PROBABLE_MATCH" ? "conf-probable" : "conf-no"; }
  function cardClass(c) { return c === "MATCH" ? "card-match" : c === "PROBABLE_MATCH" ? "card-probable" : ""; }
  function confIcon(c) { return c === "MATCH" ? "check_circle" : c === "PROBABLE_MATCH" ? "help" : "cancel"; }
  function venueClass(v) { return v === "polymarket" ? "venue-poly" : "venue-kalshi"; }
  function venueIcon(v) { return v === "polymarket" ? "P" : "K"; }

  function mktDataAttrs(m) {
    return ' data-venue="' + escHtml(m.venue) + '"' +
      ' data-market-id="' + escHtml(m.venue_market_id) + '"' +
      ' data-title="' + escHtml(m.title) + '"' +
      ' data-description="' + escHtml(m.description) + '"' +
      ' data-category="' + escHtml(m.category) + '"' +
      ' data-tags="' + escHtml(m.tags) + '"' +
      ' data-status="' + escHtml(m.status) + '"' +
      ' data-yes="' + (m.yes_price || 0).toFixed(6) + '"' +
      ' data-token-id="' + escHtml(m.venue_yes_token_id || "") + '"' +
      ' data-liquidity="' + (m.liquidity || 0).toFixed(2) + '"' +
      ' data-spread="' + (m.spread || 0).toFixed(6) + '"' +
      ' data-resolution-date="' + escHtml(m.resolution_date) + '"' +
      ' data-created-at="' + escHtml(m.created_at) + '"' +
      ' data-updated-at="' + escHtml(m.updated_at) + '"' +
      ' data-volume24h="' + (m.volume_24h || 0).toFixed(2) + '"' +
      ' data-open-interest="' + (m.open_interest || 0).toFixed(2) + '"' +
      ' data-resolution-criteria="' + escHtml(m.resolution_raw) + '"' +
      ' data-venue-link="' + escHtml(m.venue_link) + '"' +
      ' data-venue-search-link="' + escHtml(m.venue_search_link) + '"' +
      ' data-venue-search-link-alt="' + escHtml(m.venue_search_link_alt) + '"' +
      ' data-image-url="' + escHtml(m.image_url) + '"' +
      ' data-payload="' + escHtml(m.raw_payload_b64) + '"';
  }

  function renderMktCol(m) {
    var thumb = m.image_url ? '<img class="mkt-thumb" src="' + escHtml(m.image_url) + '" alt="" loading="lazy">' : '';
    return '<div class="market-col clickable-market"' + mktDataAttrs(m) + '>' +
      '<div class="mkt-header">' +
        thumb +
        '<div class="venue-dot vd-' + escHtml(m.venue) + '">' + venueIcon(m.venue) + '</div>' +
        '<div class="mkt-title">' + escHtml(m.title) + '</div>' +
      '</div>' +
      '<div class="mkt-stats">' +
        '<span class="mkt-price" data-venue="' + escHtml(m.venue) + '" data-market-id="' + escHtml(m.venue_market_id) + '" data-token-id="' + escHtml(m.venue_yes_token_id || "") + '">' + fmtPct(m.yes_price || 0) + '</span>' +
        '<span class="mkt-stat">Liq <span>' + fmtUsd(m.liquidity) + '</span></span>' +
        '<span class="mkt-stat">Spread <span>' + (m.spread ? fmtPct(m.spread) : "--") + '</span></span>' +
        (m.resolution_date ? '<span class="mkt-stat">Res <span>' + escHtml(m.resolution_date) + '</span></span>' : '') +
        (m.volume_24h >= 1000 ? '<span class="mkt-stat">24h <span>' + fmtUsd(m.volume_24h) + '</span></span>' : '') +
      '</div>' +
    '</div>';
  }

  function renderPairCard(p, idx) {
    return '<div class="pair-card ' + cardClass(p.confidence) + '" id="pair-' + idx + '" style="opacity:0;transform:translateY(16px);transition:opacity 400ms ease,transform 400ms ease;">' +
      '<div class="pair-head">' +
        '<div class="pair-index">' + (idx + 1) + '</div>' +
        '<div class="conf-badge ' + confClass(p.confidence) + '">' +
          '<span class="material-icons-round">' + confIcon(p.confidence) + '</span> ' +
          escHtml(p.confidence) +
        '</div>' +
        '<div class="head-separator"></div>' +
        '<div class="score-pills">' +
          '<div class="score-pill">Fuzzy <div class="bar-mini"><div class="bar-mini-fill" style="width:' + fmtScoreWidth(p.fuzzy_score) + '"></div></div> <strong>' + fmtScore(p.fuzzy_score) + '</strong></div>' +
          '<div class="score-pill">Embed <div class="bar-mini"><div class="bar-mini-fill" style="width:' + fmtScoreWidth(p.embedding_score) + '"></div></div> <strong>' + fmtScore(p.embedding_score) + '</strong></div>' +
          '<div class="score-pill">Composite <div class="bar-mini"><div class="bar-mini-fill" style="width:' + fmtScoreWidth(p.composite_score) + '"></div></div> <strong>' + fmtScore(p.composite_score) + '</strong></div>' +
        '</div>' +
        '<div class="head-spacer"></div>' +
        '<div class="route-chip">' +
          '<span class="material-icons-round">arrow_forward</span>' +
          '<span class="rv ' + venueClass(p.selected_venue) + '">' + escHtml(p.selected_venue) + '</span>' +
        '</div>' +
        '<button class="expand-btn" onclick="toggleExplain(this, \'explain-s' + idx + '\')">' +
          '<span class="material-icons-round">expand_more</span>' +
        '</button>' +
      '</div>' +
      '<div class="pair-body">' +
        renderMktCol(p.market_a) +
        renderMktCol(p.market_b) +
      '</div>' +
      '<div class="pair-explain" id="explain-s' + idx + '">' +
        '<div class="pair-explain-inner">' +
          '<div class="pair-explain-section"><div class="pair-explain-label">Match reasoning</div>' + escHtml(p.explanation) + '</div>' +
          '<div class="pair-explain-section"><div class="pair-explain-label">Routing decision</div>' + escHtml(p.routing_reason) + '</div>' +
        '</div>' +
      '</div>' +
    '</div>';
  }

  function bindClickableMarkets(container) {
    container.querySelectorAll(".clickable-market").forEach(function(card) {
      card.addEventListener("click", function() {
        if (typeof showMarketModal === "function") showMarketModal(card);
      });
    });
  }

  function startStreamSearch(q, deepSearch) {
    showStreamUI(q);
    var target = "/?q=" + encodeURIComponent(q) + (deepSearch ? "&more=1" : "");
    history.pushState(null, "", target);
    var pairsContainer = document.getElementById("streamPairs");
    var matchCount = 0;
    var probableCount = 0;
    var pairCount = 0;

    var searchDone = false;
    function updateHeader() {
      var header = document.getElementById("streamHeader");
      if (!header) return;
      var html = '';
      if (searchDone) {
        html = '<div class="results-title">Results for <strong>"' + escHtml(q) + '"</strong></div>';
      } else if (pairCount > 0) {
        html = '<div class="results-title"><span class="material-icons-round" style="font-size:14px;vertical-align:middle;animation:loaderSpin 0.9s linear infinite;margin-right:4px;">sync</span> Finding matches for <strong>"' + escHtml(q) + '"</strong></div>';
      } else {
        html = '<div class="results-title"><span class="material-icons-round" style="font-size:14px;vertical-align:middle;animation:loaderSpin 0.9s linear infinite;margin-right:4px;">sync</span> Searching for <strong>"' + escHtml(q) + '"</strong></div>';
      }
      if (matchCount) html += '<span class="result-badge badge-match">' + matchCount + ' matched</span>';
      if (probableCount) html += '<span class="result-badge badge-probable">' + probableCount + ' probable</span>';
      header.innerHTML = html;
    }

    var es = new EventSource("/stream?q=" + encodeURIComponent(q) + (deepSearch ? "&more=1" : ""));
    es.onmessage = function(e) {
      try {
        var evt = JSON.parse(e.data);
        if (evt.type === "step") {
          addLine("loader-step", "spin", "↻", evt.msg, "");
        } else if (evt.type === "result") {
          var extra = venueTag(evt.venue);
          if (evt.count > 0) extra = countBadge(evt.count) + (evt.venue ? venueTag(evt.venue) : "");
          addLine("loader-result", "ok", "✓", evt.msg, extra);
        } else if (evt.type === "pair" && evt.pair) {
          var idx = pairCount;
          pairCount++;
          if (evt.pair.confidence === "MATCH") matchCount++;
          else if (evt.pair.confidence === "PROBABLE_MATCH") probableCount++;
          // Hide the loader log once the first pair arrives
          if (pairCount === 1) {
            var loader = document.getElementById("streamLoader");
            if (loader) loader.style.display = "none";
          }
          var cardHtml = renderPairCard(evt.pair, idx);
          var div = document.createElement("div");
          div.innerHTML = cardHtml;
          var card = div.firstChild;
          pairsContainer.appendChild(card);
          bindClickableMarkets(card);
          requestAnimationFrame(function() {
            card.style.opacity = "1";
            card.style.transform = "translateY(0)";
          });
          updateHeader();
        } else if (evt.type === "done") {
          es.close();
          searchDone = true;
          var loader = document.getElementById("streamLoader");
          if (loader) {
            if (pairCount === 0) {
              // Move empty state above (replace loader area)
              loader.innerHTML =
                '<div class="empty-state">' +
                  '<div class="empty-icon material-icons-round">search_off</div>' +
                  '<div class="empty-title">No equivalent pairs found</div>' +
                  '<div class="empty-sub">Try a different search query, or adjust MATCH_THRESHOLD / MAX_DATE_DELTA_DAYS to widen the match window.</div>' +
                '</div>';
            } else {
              loader.style.display = "none";
            }
          }
          updateHeader();
        } else if (evt.type === "error") {
          es.close();
          addLine("loader-result", "warn", "!", evt.msg, "");
        }
      } catch(ex) {}
    };
    es.onerror = function() {
      es.close();
      var loader = document.getElementById("streamLoader");
      if (loader && pairCount === 0) {
        loader.innerHTML =
          '<div class="empty-state">' +
            '<div class="empty-icon material-icons-round">error_outline</div>' +
            '<div class="empty-title">Connection lost</div>' +
            '<div class="empty-sub">The search was interrupted. Please try again.</div>' +
          '</div>';
      } else if (loader) {
        loader.style.display = "none";
      }
      updateHeader();
    };
  }

  document.querySelectorAll("form").forEach(function(form) {
    form.addEventListener("submit", function(e) {
      var input = form.querySelector("[name=q]");
      if (!input) return;
      var q = input.value.trim();
      if (!q) return;
      e.preventDefault();
      startStreamSearch(q, false);
    });
  });

  // Intercept hint links (they navigate directly, skip them to stay SSE-driven)
  document.querySelectorAll(".hero-hint").forEach(function(a) {
    a.addEventListener("click", function(e) {
      e.preventDefault();
      var url = new URL(a.href);
      var q = url.searchParams.get("q") || "";
      if (q) startStreamSearch(q, false);
    });
  });

})();
</script>
</body>
</html>
`))
