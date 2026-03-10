// Command equinox-ui runs the full Equinox pipeline and serves a local web UI
// showing matched market pairs side by side with routing decisions.
//
// Usage:
//
//	OPENAI_API_KEY=sk-... go run ./cmd/equinox-ui
//
// Then open http://localhost:8080 in your browser.
package main

import (
	"encoding/base64"
	"encoding/json"
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/kalshisearch"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/normalizer"
	"github.com/equinox/internal/router"
	"github.com/equinox/internal/store"
	"github.com/equinox/internal/venues"
	"github.com/equinox/internal/venues/kalshi"
	"github.com/equinox/internal/venues/polymarket"
)

const refreshInterval = 10 * time.Minute

// defaultSeedQueries are high-overlap topics that both Polymarket and Kalshi
// commonly list markets for. Used on the home page when no search query is given,
// so the user sees real matched pairs immediately.
var defaultSeedQueries = []string{
	"Trump",
	"Bitcoin",
	"Fed rate",
	"recession",
}

const seedPerVenueLimit = 5  // small per topic — we just need enough for good cross-venue matches
const maxDisplayPairs = 20   // cap pairs shown in UI

// PageData is passed to the HTML template.
type PageData struct {
	RunAt            string
	TotalMarkets     int
	VenueCounts      map[models.VenueID]int
	Pairs            []PairView
	MatchCount       int // MATCH confidence only
	ProbableCount    int // PROBABLE_MATCH confidence only
	SearchQuery      string
	IsHomePage       bool // true when showing seed-query results (no explicit user query)
	Loading          bool // true when seed cache is still warming up
	NearMisses       []NearMissView // top rejected pairs for diagnosis
	DiagnosisMessage string         // human-readable explanation of why no matches were found
}

// NearMissView is a rejected pair shown when no matches are found.
type NearMissView struct {
	TitleA         string
	TitleB         string
	VenueA         string
	VenueB         string
	FuzzyScore     float64
	EmbeddingScore float64
	CompositeScore float64
	DatePenalty    float64
	Reason         string
}

// seedCache holds precomputed home page data so it loads instantly.
type seedCache struct {
	mu   sync.RWMutex
	data *PageData // nil while warming up
}

const uiPerVenueLimit = 100

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
	Title              string  `json:"title"`
	Category           string  `json:"category"`
	Status             string  `json:"status"`
	Description        string  `json:"description"`
	Tags               string  `json:"tags"`
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
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	// Enable embedding cache by default in UI mode so repeated page loads are instant.
	// The CLI already has this as opt-in; the UI always benefits from caching.
	if !cfg.EmbeddingCacheEnabled && cfg.OpenAIAPIKey != "" {
		cfg.EmbeddingCacheEnabled = true
		if cfg.EmbeddingCachePath == "" {
			cfg.EmbeddingCachePath = ".equinox_embedding_cache.json"
		}
		fmt.Println("[equinox-ui] Embedding cache auto-enabled for UI mode")
	}

	// Open SQLite store for persisting homepage cache across restarts.
	db, err := store.Open("")
	if err != nil {
		log.Fatalf("opening cache store: %v", err)
	}
	defer db.Close()

	// Try to load cached homepage data from SQLite on startup.
	cache := &seedCache{}
	if cached, err := db.LoadHomepage(); err == nil && cached != nil {
		age := db.Age()
		page := cachedToPageData(cached)
		cache.mu.Lock()
		cache.data = page
		cache.mu.Unlock()
		fmt.Printf("[equinox-ui] Loaded homepage from SQLite cache (%d pairs, age %s)\n",
			len(page.Pairs), age.Round(time.Second))
	}

	// Background worker: refresh seed data and persist to SQLite every 10 minutes.
	go func() {
		// If no cached data, run immediately. Otherwise wait for next interval.
		if cache.data == nil {
			fmt.Println("[equinox-ui] No cached data — running seed pipeline now...")
			refreshSeedCache(cfg, cache, db)
		} else {
			// Still refresh soon to get fresh data, but don't block startup.
			fmt.Println("[equinox-ui] Refreshing seed data in background...")
			refreshSeedCache(cfg, cache, db)
		}

		ticker := time.NewTicker(refreshInterval)
		for range ticker.C {
			fmt.Println("[equinox-ui] Background refresh (10 min interval)...")
			refreshSeedCache(cfg, cache, db)
		}
	}()

	fmt.Println("[equinox-ui] Serving at http://localhost:8080")
	kalshiSearch := kalshisearch.New(cfg.HTTPTimeout)

	http.HandleFunc("/api/kalshi-search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		if status == "" {
			status = "open"
		}
		limit := parseIntWithDefault(r.URL.Query().Get("limit"), 20)
		if limit > 100 {
			limit = 100
		}
		if limit <= 0 {
			limit = 20
		}
		fmt.Printf("[kalshi-search] request q=%q status=%q limit=%d type=%q series=%q event=%q\n",
			q, status, limit,
			strings.TrimSpace(r.URL.Query().Get("type")),
			strings.TrimSpace(r.URL.Query().Get("series")),
			strings.TrimSpace(r.URL.Query().Get("event")))

		resp, err := kalshiSearch.Search(r.Context(), kalshisearch.SearchOptions{
			Query:  q,
			Status: status,
			Limit:  limit,
			Type:   strings.TrimSpace(r.URL.Query().Get("type")),
			Series: strings.TrimSpace(r.URL.Query().Get("series")),
			Event:  strings.TrimSpace(r.URL.Query().Get("event")),
		})
		if err != nil {
			fmt.Printf("[kalshi-search] error q=%q: %v\n", q, err)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"query":   q,
				"count":   0,
				"results": []interface{}{},
				"error":   err.Error(),
			})
			return
		}
		fmt.Printf("[kalshi-search] response q=%q count=%d refreshed=%v warnings=%d\n",
			q, resp.Count, resp.Refreshed, len(resp.Warnings))

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		var data *PageData
		var err error
		if query == "" {
			// Serve from precomputed cache — instant response
			cache.mu.RLock()
			cached := cache.data
			cache.mu.RUnlock()

			if cached != nil {
				data = cached
				fmt.Println("[equinox-ui] Home page served from cache")
			} else {
				// Cache still warming up — show loading page
				data = &PageData{
					RunAt:       time.Now().Format("2006-01-02 15:04:05"),
					VenueCounts: map[models.VenueID]int{},
					IsHomePage:  true,
					Loading:     true,
				}
				fmt.Println("[equinox-ui] Home page — cache still warming up")
			}
		} else {
			fmt.Printf("[equinox-ui] Running search pipeline q=%q\n", query)
			data, err = runSearchPipeline(cfg, query)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		fmt.Printf("[equinox-ui] Rendering: %d pairs\n", len(data.Pairs))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := pageTmpl.Execute(w, data); err != nil {
			fmt.Printf("[equinox-ui] ERROR: rendering template: %v\n", err)
		}
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}

func parseIntWithDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// runSeedPipeline fetches markets for each seed query from both venues in parallel,
// deduplicates, then normalizes everything in a single batch (one embedding API call).
// This populates the home page with real cross-venue matches.
func runSeedPipeline(cfg *config.Config) (*PageData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	polyClient := polymarket.New(cfg.HTTPTimeout, seedPerVenueLimit)
	kalshiClient := kalshi.New(cfg.KalshiAPIKey, cfg.HTTPTimeout, seedPerVenueLimit)

	// Phase 1: Fire all seed queries in parallel (4 queries × 2 venues = 8 goroutines)
	type fetchResult struct {
		query string
		venue string
		raw   []*venues.RawMarket
		err   error
	}

	results := make(chan fetchResult, len(defaultSeedQueries)*2)
	var wg sync.WaitGroup

	for _, q := range defaultSeedQueries {
		wg.Add(2)
		go func(query string) {
			defer wg.Done()
			raw, err := polyClient.FetchMarketsByQuery(ctx, query)
			results <- fetchResult{query: query, venue: "polymarket", raw: raw, err: err}
		}(q)
		go func(query string) {
			defer wg.Done()
			raw, err := kalshiClient.FetchMarketsByQuery(ctx, query)
			results <- fetchResult{query: query, venue: "kalshi", raw: raw, err: err}
		}(q)
	}

	go func() { wg.Wait(); close(results) }()

	seenRaw := map[string]bool{}
	var allRaw []*venues.RawMarket
	for r := range results {
		if r.err != nil {
			fmt.Printf("[equinox-ui] WARNING: %s seed %q: %v\n", r.venue, r.query, r.err)
			continue
		}
		added := 0
		for _, rm := range r.raw {
			key := string(rm.VenueID) + ":" + rm.VenueMarketID
			if seenRaw[key] {
				continue
			}
			seenRaw[key] = true
			allRaw = append(allRaw, rm)
			added++
		}
		fmt.Printf("[equinox-ui] Seed %q/%s: %d markets (%d new)\n", r.query, r.venue, len(r.raw), added)
	}

	// Phase 2: Single normalize call = single embedding API batch
	norm := normalizer.New(cfg)
	allMarkets, err := norm.Normalize(ctx, allRaw)
	if err != nil {
		return nil, fmt.Errorf("normalizing seed markets: %w", err)
	}

	venueCounts := map[models.VenueID]int{}
	for _, m := range allMarkets {
		venueCounts[m.VenueID]++
	}

	fmt.Printf("[equinox-ui] Seed fetch complete: %d unique markets (poly=%d, kalshi=%d)\n",
		len(allMarkets), venueCounts[models.VenuePolymarket], venueCounts[models.VenueKalshi])
	return matchAndRoute(cfg, ctx, allMarkets, venueCounts, "", true)
}

// refreshSeedCache runs the seed pipeline, updates the in-memory cache, and
// persists the result to SQLite so it survives restarts.
func refreshSeedCache(cfg *config.Config, cache *seedCache, db *store.Store) {
	data, err := runSeedPipeline(cfg)
	if err != nil {
		fmt.Printf("[equinox-ui] WARNING: seed pipeline failed: %v\n", err)
		return
	}

	// Update in-memory cache.
	cache.mu.Lock()
	cache.data = data
	cache.mu.Unlock()
	fmt.Printf("[equinox-ui] Seed cache updated: %d pairs\n", len(data.Pairs))

	// Persist to SQLite.
	pairsJSON, err := json.Marshal(data.Pairs)
	if err != nil {
		fmt.Printf("[equinox-ui] WARNING: failed to marshal pairs for SQLite: %v\n", err)
		return
	}
	venueCounts := make(map[string]int, len(data.VenueCounts))
	for k, v := range data.VenueCounts {
		venueCounts[string(k)] = v
	}
	if err := db.SaveHomepage(&store.CachedPageData{
		RunAt:         data.RunAt,
		TotalMarkets:  data.TotalMarkets,
		VenueCounts:   venueCounts,
		Pairs:         pairsJSON,
		MatchCount:    data.MatchCount,
		ProbableCount: data.ProbableCount,
	}); err != nil {
		fmt.Printf("[equinox-ui] WARNING: failed to save to SQLite: %v\n", err)
	} else {
		fmt.Println("[equinox-ui] Homepage data persisted to SQLite")
	}
}

// cachedToPageData converts a SQLite-loaded CachedPageData back into a PageData.
func cachedToPageData(c *store.CachedPageData) *PageData {
	var pairs []PairView
	if err := json.Unmarshal(c.Pairs, &pairs); err != nil {
		fmt.Printf("[equinox-ui] WARNING: failed to unmarshal cached pairs: %v\n", err)
	}
	venueCounts := make(map[models.VenueID]int, len(c.VenueCounts))
	for k, v := range c.VenueCounts {
		venueCounts[models.VenueID(k)] = v
	}
	matchCount := 0
	probableCount := 0
	for _, p := range pairs {
		switch p.Confidence {
		case "MATCH":
			matchCount++
		case "PROBABLE_MATCH":
			probableCount++
		}
	}
	return &PageData{
		RunAt:         c.RunAt,
		TotalMarkets:  c.TotalMarkets,
		VenueCounts:   venueCounts,
		Pairs:         pairs,
		MatchCount:    matchCount,
		ProbableCount: probableCount,
		IsHomePage:    true,
	}
}

// runSearchPipeline fetches markets matching a single user query from both venues.
func runSearchPipeline(cfg *config.Config, query string) (*PageData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	polyClient := polymarket.New(cfg.HTTPTimeout, uiPerVenueLimit)
	kalshiClient := kalshi.New(cfg.KalshiAPIKey, cfg.HTTPTimeout, uiPerVenueLimit)

	// Fetch from both venues in parallel
	fmt.Printf("[equinox-ui] Fetching from both venues q=%q...\n", query)
	var polyRaw, kalshiRaw []*venues.RawMarket
	var polyErr, kalshiErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		polyRaw, polyErr = polyClient.FetchMarketsByQuery(ctx, query)
	}()
	go func() {
		defer wg.Done()
		kalshiRaw, kalshiErr = kalshiClient.FetchMarketsByQuery(ctx, query)
	}()
	wg.Wait()

	if polyErr != nil {
		fmt.Printf("[equinox-ui] WARNING: skipping polymarket: %v\n", polyErr)
	}
	if kalshiErr != nil {
		fmt.Printf("[equinox-ui] WARNING: skipping kalshi: %v\n", kalshiErr)
	}

	// Single normalize call = single embedding API batch
	allRaw := append(polyRaw, kalshiRaw...)
	norm := normalizer.New(cfg)
	allMarkets, err := norm.Normalize(ctx, allRaw)
	if err != nil {
		return nil, fmt.Errorf("normalizing search results: %w", err)
	}

	venueCounts := map[models.VenueID]int{}
	for _, m := range allMarkets {
		venueCounts[m.VenueID]++
	}
	fmt.Printf("[equinox-ui] Search results: poly=%d kalshi=%d\n",
		venueCounts[models.VenuePolymarket], venueCounts[models.VenueKalshi])

	return matchAndRoute(cfg, ctx, allMarkets, venueCounts, query, false)
}

// matchAndRoute runs the matcher and router on a set of canonical markets and
// builds the PageData for the template.
func matchAndRoute(cfg *config.Config, ctx context.Context, allMarkets []*models.CanonicalMarket, venueCounts map[models.VenueID]int, query string, isHomePage bool) (*PageData, error) {
	totalEmb := 0
	for _, m := range allMarkets {
		if len(m.TitleEmbedding) > 0 {
			totalEmb++
		}
	}
	fmt.Printf("[equinox-ui] total embeddings: %d/%d\n", totalEmb, len(allMarkets))

	var openaiClient *openai.Client
	if cfg.OpenAIAPIKey != "" {
		openaiClient = openai.NewClient(cfg.OpenAIAPIKey)
	}

	m := matcher.New(cfg, openaiClient)
	allPairs := m.FindEquivalentPairs(ctx, allMarkets)
	// Cap displayed pairs — they're already sorted by score descending
	pairs := allPairs
	if len(pairs) > maxDisplayPairs {
		pairs = pairs[:maxDisplayPairs]
	}
	var nearMisses []NearMissView
	var diagnosisMsg string
	if len(pairs) == 0 {
		rejected := m.TopRejectedPairs(allMarkets, 5)
		if len(rejected) == 0 {
			fmt.Println("[equinox-ui] debug: no rejected cross-venue candidates available")
		} else {
			fmt.Println("[equinox-ui] debug: top rejected cross-venue candidates:")
			for i, rj := range rejected {
				fmt.Printf("[equinox-ui] reject #%d score=%.3f fuzzy=%.3f emb=%.3f | A=%q | B=%q | reason=%s\n",
					i+1, rj.CompositeScore, rj.FuzzyScore, rj.EmbeddingScore,
					rj.MarketA.Title, rj.MarketB.Title, rj.Explanation)
				nearMisses = append(nearMisses, NearMissView{
					TitleA:         rj.MarketA.Title,
					TitleB:         rj.MarketB.Title,
					VenueA:         string(rj.MarketA.VenueID),
					VenueB:         string(rj.MarketB.VenueID),
					FuzzyScore:     rj.FuzzyScore,
					EmbeddingScore: rj.EmbeddingScore,
					CompositeScore: rj.CompositeScore,
					DatePenalty:    rj.DatePenalty,
					Reason:         rj.Explanation,
				})
			}
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

	matchCount := 0
	probableCount := 0
	for _, pv := range pairViews {
		switch pv.Confidence {
		case "MATCH":
			matchCount++
		case "PROBABLE_MATCH":
			probableCount++
		}
	}

	return &PageData{
		RunAt:            time.Now().Format("2006-01-02 15:04:05"),
		TotalMarkets:     len(allMarkets),
		VenueCounts:      venueCounts,
		Pairs:            pairViews,
		MatchCount:       matchCount,
		ProbableCount:    probableCount,
		SearchQuery:      query,
		IsHomePage:       isHomePage,
		NearMisses:       nearMisses,
		DiagnosisMessage: diagnosisMsg,
	}, nil
}

// buildDiagnosis generates a human-readable explanation of why no matches were found.
func buildDiagnosis(venueCounts map[models.VenueID]int, rejected []*matcher.MatchResult) string {
	polyCount := venueCounts[models.VenuePolymarket]
	kalshiCount := venueCounts[models.VenueKalshi]

	// Case 1: one venue returned nothing
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

	// Analyze rejection reasons
	datePenalized := 0
	lowSemantic := 0
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
		parts = append(parts, "The market titles have low semantic similarity \u2014 the venues appear to be asking fundamentally different questions about this topic (e.g., one asks about seat counts while the other asks who wins).")
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
	createdAt := ""
	updatedAt := ""
	if !m.CreatedAt.IsZero() {
		createdAt = m.CreatedAt.Format("2006-01-02 15:04:05")
	}
	if !m.UpdatedAt.IsZero() {
		updatedAt = m.UpdatedAt.Format("2006-01-02 15:04:05")
	}
	return MarketView{
		Venue:          string(m.VenueID),
		VenueMarketID:  m.VenueMarketID,
		Title:          m.Title,
		Category:       m.Category,
		Status:         string(m.Status),
		Description:    m.Description,
		Tags:           strings.Join(m.Tags, ", "),
		YesPrice:       m.YesPrice,
		Liquidity:      m.Liquidity,
		Spread:         m.Spread,
		ResolutionDate:  res,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
		Volume24h:      m.Volume24h,
		OpenInterest:   m.OpenInterest,
		ResolutionRaw:   m.ResolutionCriteria,
		RawPayloadB64:   base64.StdEncoding.EncodeToString(m.RawPayload),
		VenueLink:       marketVenueLink(m),
		VenueSearchLink:  marketVenueSearchLink(m),
		VenueSearchLinkAlt: marketVenueSearchLinkAlt(m),
	}
}

func marketVenueLink(m *models.CanonicalMarket) string {
	switch m.VenueID {
	case models.VenuePolymarket:
		// Polymarket uses /event/<event-slug>/<market-slug> format.
		slug := m.VenueSlug
		if slug == "" {
			slug = m.VenueMarketID
		}
		if m.VenueEventTicker != "" {
			return "https://polymarket.com/event/" + url.PathEscape(m.VenueEventTicker) + "/" + url.PathEscape(slug)
		}
		// Fallback: event-only URL (works when event slug == market slug)
		return "https://polymarket.com/event/" + url.PathEscape(slug)
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
	case models.VenuePolymarket:
		return ""
	case models.VenueKalshi:
		if m.VenueMarketID == "" {
			return ""
		}
		ticker := url.QueryEscape(strings.ToLower(m.VenueMarketID))
		return "https://kalshi.com/browse?search=" + ticker
	default:
		return ""
	}
}

func marketVenueKalshiLink(m *models.CanonicalMarket) string {
	// Kalshi frontend URL format: /markets/{series_ticker}/{title_slug}/{event_ticker}
	seriesTicker := strings.TrimSpace(strings.ToLower(m.VenueSeriesTicker))
	eventTicker := strings.TrimSpace(strings.ToLower(m.VenueEventTicker))
	eventTitle := strings.TrimSpace(m.VenueEventTitle)

	if seriesTicker != "" && eventTicker != "" && eventTitle != "" {
		slug := kalshiTitleSlug(eventTitle)
		return "https://kalshi.com/markets/" + url.PathEscape(seriesTicker) + "/" + slug + "/" + url.PathEscape(eventTicker)
	}
	// Fallback: search by market title (tickers don't work in Kalshi's search UI)
	if m.Title != "" {
		return "https://kalshi.com/browse?search=" + url.QueryEscape(m.Title)
	}
	return ""
}

// kalshiTitleSlug converts an event title to a URL slug matching Kalshi's frontend format.
func kalshiTitleSlug(title string) string {
	s := strings.ToLower(title)
	// Replace non-alphanumeric characters with hyphens
	var buf strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			buf.WriteRune(r)
		} else {
			buf.WriteByte('-')
		}
	}
	// Collapse multiple hyphens and trim
	result := buf.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	return strings.Trim(result, "-")
}

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"pct": func(f float64) string { return fmt.Sprintf("%.1f%%", f*100) },
	"pctVal": func(f float64) string { return fmt.Sprintf("%.1f", f*100) },
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
	"score": func(f float64) string { return fmt.Sprintf("%.3f", f) },
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
	"inc": func(i int) int { return i + 1 },
	"priceDiff": func(a, b float64) string {
		diff := (a - b) * 100
		if diff > 0 {
			return fmt.Sprintf("+%.1f", diff)
		}
		return fmt.Sprintf("%.1f", diff)
	},
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Equinox - Cross-Venue Market Intelligence</title>
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
  --radius: 12px;
  --radius-sm: 8px;
  --radius-xs: 6px;
}
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: 'Inter', -apple-system, BlinkMacSystemFont, sans-serif; background: var(--bg-primary); color: var(--text-primary); line-height: 1.5; -webkit-font-smoothing: antialiased; }

/* Header */
.header { position: sticky; top: 0; z-index: 50; background: rgba(6, 8, 13, 0.85); backdrop-filter: blur(20px) saturate(180%); border-bottom: 1px solid var(--border); }
.header-inner { max-width: 1400px; margin: 0 auto; padding: 16px 28px; display: flex; align-items: center; justify-content: space-between; gap: 20px; flex-wrap: wrap; }
.logo { display: flex; align-items: center; gap: 10px; text-decoration: none; }
.logo-icon { width: 36px; height: 36px; border-radius: 10px; background: linear-gradient(135deg, var(--accent), #6366f1); display: flex; align-items: center; justify-content: center; font-weight: 800; font-size: 16px; color: white; }
.logo-text { font-size: 1.25rem; font-weight: 700; color: var(--text-primary); letter-spacing: -0.5px; }
.logo-text small { font-size: 0.7rem; font-weight: 400; color: var(--text-muted); margin-left: 8px; letter-spacing: 0.02em; }
.search-form { display: flex; align-items: center; gap: 8px; }
.search-input { width: 340px; max-width: 50vw; padding: 10px 16px; background: var(--bg-card); border: 1px solid var(--border); border-radius: 999px; color: var(--text-primary); font-size: 0.85rem; font-family: inherit; transition: all 200ms; }
.search-input:focus { outline: none; border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-glow); }
.search-input::placeholder { color: var(--text-muted); }
.search-btn { padding: 10px 20px; background: linear-gradient(135deg, #6366f1, #818cf8); border: none; border-radius: 999px; color: white; font-size: 0.85rem; font-weight: 600; font-family: inherit; cursor: pointer; transition: all 200ms; }
.search-btn:hover { transform: translateY(-1px); box-shadow: 0 4px 20px rgba(99, 102, 241, 0.3); }

/* Stats bar */
.stats-bar { background: var(--bg-secondary); border-bottom: 1px solid var(--border); }
.stats-inner { max-width: 1400px; margin: 0 auto; padding: 14px 28px; display: flex; align-items: center; gap: 32px; flex-wrap: wrap; }
.stat-item { display: flex; align-items: center; gap: 8px; }
.stat-value { font-size: 1.4rem; font-weight: 700; background: linear-gradient(135deg, var(--accent-bright), var(--accent)); -webkit-background-clip: text; -webkit-text-fill-color: transparent; background-clip: text; }
.stat-label { font-size: 0.72rem; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.06em; font-weight: 500; }
.stat-divider { width: 1px; height: 28px; background: var(--border); }
.stat-badge { display: inline-flex; align-items: center; gap: 4px; padding: 4px 10px; border-radius: 999px; font-size: 0.72rem; font-weight: 600; }
.stat-badge-match { background: var(--green-bg); color: var(--green); border: 1px solid var(--green-border); }
.stat-badge-probable { background: var(--yellow-bg); color: var(--yellow); border: 1px solid var(--yellow-border); }
.run-meta { margin-left: auto; font-size: 0.75rem; color: var(--text-muted); display: flex; align-items: center; gap: 4px; }

/* Main content */
.content { max-width: 1400px; margin: 0 auto; padding: 24px 28px; }

/* Loading state */
.loading-state { display: flex; flex-direction: column; align-items: center; justify-content: center; padding: 100px 20px; }
.loading-spinner { width: 48px; height: 48px; border: 3px solid var(--border); border-top-color: var(--accent); border-radius: 50%; animation: spin 0.8s linear infinite; margin-bottom: 20px; }
@keyframes spin { to { transform: rotate(360deg); } }
.loading-text { font-size: 1.1rem; font-weight: 600; color: var(--text-primary); margin-bottom: 4px; }
.loading-sub { font-size: 0.85rem; color: var(--text-muted); }

/* Empty state */
.empty-state { text-align: center; padding: 80px 20px; }
.empty-icon { font-size: 3rem; color: var(--text-muted); margin-bottom: 16px; }
.empty-title { font-size: 1.2rem; font-weight: 600; margin-bottom: 8px; }
.empty-sub { font-size: 0.88rem; color: var(--text-muted); max-width: 460px; margin: 0 auto; }

/* Diagnosis section */
.diagnosis { margin-top: 24px; text-align: left; max-width: 720px; margin-left: auto; margin-right: auto; }
.diagnosis-box { background: var(--bg-card); border: 1px solid var(--border); border-radius: var(--radius); padding: 20px; margin-bottom: 16px; }
.diagnosis-label { font-size: 0.72rem; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase; color: var(--text-muted); margin-bottom: 8px; }
.diagnosis-msg { font-size: 0.88rem; color: var(--text-secondary); line-height: 1.5; }
.near-miss-title { font-size: 0.72rem; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase; color: var(--text-muted); margin-bottom: 12px; }
.near-miss-card { background: var(--bg-elevated); border: 1px solid var(--border); border-radius: var(--radius-sm); padding: 14px 16px; margin-bottom: 10px; }
.near-miss-titles { display: flex; gap: 8px; align-items: flex-start; margin-bottom: 8px; }
.near-miss-vs { color: var(--text-muted); font-size: 0.72rem; font-weight: 600; flex-shrink: 0; padding-top: 2px; }
.near-miss-t { font-size: 0.82rem; color: var(--text-primary); flex: 1; line-height: 1.35; }
.near-miss-venue { font-size: 0.68rem; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.03em; }
.near-miss-scores { display: flex; gap: 10px; flex-wrap: wrap; }
.near-miss-pill { font-size: 0.72rem; color: var(--text-muted); background: var(--bg-card); padding: 2px 8px; border-radius: 999px; }
.near-miss-pill strong { color: var(--text-secondary); font-weight: 600; }
.near-miss-reason { font-size: 0.75rem; color: var(--text-muted); margin-top: 6px; font-style: italic; }

/* Pair card */
.pair-card { background: var(--bg-card); border: 1px solid var(--border); border-radius: var(--radius); margin-bottom: 20px; overflow: hidden; transition: border-color 200ms, box-shadow 200ms; }
.pair-card:hover { border-color: var(--border-hover); box-shadow: 0 8px 40px rgba(0, 0, 0, 0.2); }

/* Pair header */
.pair-head { display: flex; align-items: center; gap: 12px; padding: 14px 20px; border-bottom: 1px solid var(--border); }
.pair-index { width: 28px; height: 28px; border-radius: var(--radius-xs); background: var(--bg-elevated); display: flex; align-items: center; justify-content: center; font-size: 0.75rem; font-weight: 700; color: var(--text-muted); flex-shrink: 0; }
.conf-badge { display: inline-flex; align-items: center; gap: 5px; padding: 5px 12px; border-radius: 999px; font-size: 0.73rem; font-weight: 600; letter-spacing: 0.02em; }
.conf-badge .material-icons-round { font-size: 14px; }
.conf-match { background: var(--green-bg); color: var(--green); border: 1px solid var(--green-border); }
.conf-probable { background: var(--yellow-bg); color: var(--yellow); border: 1px solid var(--yellow-border); }
.conf-no { background: rgba(248, 113, 113, 0.1); color: var(--red); border: 1px solid rgba(248, 113, 113, 0.2); }
.score-pills { display: flex; gap: 8px; margin-left: auto; flex-wrap: wrap; }
.score-pill { display: flex; align-items: center; gap: 5px; padding: 4px 10px; background: var(--bg-elevated); border-radius: 999px; font-size: 0.72rem; color: var(--text-secondary); }
.score-pill strong { color: var(--text-primary); font-weight: 600; }
.score-pill .bar-mini { width: 32px; height: 4px; background: rgba(255,255,255,0.06); border-radius: 2px; overflow: hidden; }
.score-pill .bar-mini-fill { height: 100%; background: var(--accent); border-radius: 2px; transition: width 600ms ease; }

/* Market columns */
.pair-body { display: grid; grid-template-columns: 1fr 1fr; }
.market-col { padding: 18px 20px; cursor: pointer; transition: background 150ms; position: relative; }
.market-col:first-child { border-right: 1px solid var(--border); }
.market-col:hover { background: var(--bg-card-hover); }

.venue-chip { display: inline-flex; align-items: center; gap: 6px; padding: 3px 10px 3px 4px; border-radius: 999px; font-size: 0.7rem; font-weight: 600; text-transform: uppercase; letter-spacing: 0.04em; margin-bottom: 10px; }
.venue-chip-icon { width: 20px; height: 20px; border-radius: 50%; display: flex; align-items: center; justify-content: center; font-size: 0.65rem; font-weight: 800; color: white; }
.venue-poly { background: var(--poly-bg); color: var(--poly-color); border: 1px solid var(--poly-border); }
.venue-poly .venue-chip-icon { background: var(--poly-color); }
.venue-kalshi { background: var(--kalshi-bg); color: var(--kalshi-color); border: 1px solid var(--kalshi-border); }
.venue-kalshi .venue-chip-icon { background: var(--kalshi-color); }

.market-title { font-size: 0.95rem; font-weight: 600; color: var(--text-primary); line-height: 1.45; margin-bottom: 14px; }

/* Price display */
.price-row { display: flex; align-items: center; gap: 12px; margin-bottom: 12px; }
.price-big { font-size: 1.7rem; font-weight: 800; letter-spacing: -1px; }
.price-big.yes-price { background: linear-gradient(135deg, var(--green), #6ee7b7); -webkit-background-clip: text; -webkit-text-fill-color: transparent; background-clip: text; }
.price-label { font-size: 0.7rem; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.05em; font-weight: 500; }
.price-bar { height: 6px; background: rgba(255,255,255,0.04); border-radius: 3px; overflow: hidden; margin-bottom: 14px; }
.price-bar-fill { height: 100%; border-radius: 3px; background: linear-gradient(90deg, var(--accent), var(--green)); transition: width 800ms ease; }

/* Meta grid */
.meta-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
.meta-cell { padding: 8px 10px; background: var(--bg-primary); border-radius: var(--radius-xs); border: 1px solid rgba(255,255,255,0.03); }
.meta-cell-label { font-size: 0.65rem; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.05em; font-weight: 500; margin-bottom: 2px; }
.meta-cell-value { font-size: 0.85rem; font-weight: 600; color: var(--text-primary); }

/* Routing footer */
.pair-footer { display: flex; align-items: center; gap: 16px; padding: 12px 20px; background: var(--bg-primary); border-top: 1px solid var(--border); }
.route-label { font-size: 0.78rem; color: var(--text-muted); }
.route-venue { display: inline-flex; align-items: center; gap: 6px; padding: 4px 12px; border-radius: 999px; font-size: 0.78rem; font-weight: 700; }
.route-venue.venue-poly { background: var(--poly-bg); color: var(--poly-color); border: 1px solid var(--poly-border); }
.route-venue.venue-kalshi { background: var(--kalshi-bg); color: var(--kalshi-color); border: 1px solid var(--kalshi-border); }
.route-arrow { color: var(--green); font-size: 16px; }
.route-reason { font-size: 0.73rem; color: var(--text-muted); margin-left: auto; max-width: 50%; text-align: right; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; cursor: help; }

/* Expandable explanation */
.pair-explain { max-height: 0; overflow: hidden; transition: max-height 300ms ease; }
.pair-explain.is-open { max-height: 400px; }
.pair-explain-inner { padding: 14px 20px; border-top: 1px solid var(--border); font-size: 0.78rem; color: var(--text-secondary); line-height: 1.6; white-space: pre-wrap; font-family: 'Inter', sans-serif; background: rgba(0,0,0,0.2); }
.expand-btn { background: none; border: none; color: var(--text-muted); cursor: pointer; font-size: 0.72rem; font-family: inherit; display: flex; align-items: center; gap: 4px; padding: 4px 8px; border-radius: var(--radius-xs); transition: all 150ms; }
.expand-btn:hover { color: var(--accent); background: var(--accent-glow); }
.expand-btn .material-icons-round { font-size: 16px; transition: transform 200ms; }
.expand-btn.is-open .material-icons-round { transform: rotate(180deg); }

/* Modal */
.modal-overlay { position: fixed; inset: 0; display: none; align-items: center; justify-content: center; z-index: 100; }
.modal-overlay.is-open { display: flex; }
.modal-bg { position: absolute; inset: 0; background: rgba(0, 0, 0, 0.7); backdrop-filter: blur(8px); }
.modal-container { position: relative; width: min(960px, 94vw); max-height: 90vh; background: var(--bg-card); border: 1px solid var(--border-hover); border-radius: var(--radius); z-index: 1; box-shadow: 0 25px 80px rgba(0, 0, 0, 0.5); display: flex; flex-direction: column; animation: modalIn 200ms ease; }
@keyframes modalIn { from { opacity: 0; transform: translateY(10px) scale(0.98); } to { opacity: 1; transform: none; } }
.modal-header { padding: 18px 24px; border-bottom: 1px solid var(--border); display: flex; align-items: center; justify-content: space-between; flex-shrink: 0; }
.modal-header-title { font-size: 1rem; font-weight: 700; color: var(--text-primary); }
.modal-close-btn { width: 32px; height: 32px; border-radius: 8px; background: var(--bg-elevated); border: 1px solid var(--border); color: var(--text-secondary); display: flex; align-items: center; justify-content: center; cursor: pointer; transition: all 150ms; }
.modal-close-btn:hover { background: rgba(248, 113, 113, 0.1); color: var(--red); border-color: rgba(248, 113, 113, 0.3); }
.modal-close-btn .material-icons-round { font-size: 18px; }
.modal-scroll { padding: 20px 24px; overflow-y: auto; flex: 1; }
.modal-section { margin-bottom: 20px; }
.modal-section-title { font-size: 0.72rem; font-weight: 600; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.06em; margin-bottom: 10px; padding-bottom: 6px; border-bottom: 1px solid var(--border); }
.modal-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
.modal-field { padding: 10px 12px; background: var(--bg-primary); border-radius: var(--radius-xs); border: 1px solid rgba(255,255,255,0.03); }
.modal-field-label { font-size: 0.68rem; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.04em; margin-bottom: 3px; }
.modal-field-value { font-size: 0.85rem; color: var(--text-primary); font-weight: 500; word-break: break-word; }
.modal-field.full { grid-column: 1 / -1; }
.raw-json { border: 1px solid var(--border); border-radius: var(--radius-sm); padding: 14px; background: var(--bg-primary); max-height: 280px; overflow: auto; white-space: pre-wrap; font-family: 'SF Mono', 'Fira Code', 'Cascadia Code', monospace; font-size: 0.72rem; color: var(--text-secondary); line-height: 1.6; }
.modal-links { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 12px; }
.modal-link { display: inline-flex; align-items: center; gap: 5px; padding: 8px 14px; background: var(--bg-elevated); border: 1px solid var(--border); border-radius: var(--radius-xs); color: var(--accent-bright); font-size: 0.8rem; font-weight: 500; text-decoration: none; transition: all 150ms; }
.modal-link:hover { border-color: var(--accent); background: var(--accent-glow); }
.modal-link .material-icons-round { font-size: 16px; }

@media (max-width: 768px) {
  .pair-body { grid-template-columns: 1fr; }
  .market-col:first-child { border-right: none; border-bottom: 1px solid var(--border); }
  .header-inner { padding: 12px 16px; }
  .content { padding: 16px; }
  .modal-grid { grid-template-columns: 1fr; }
  .score-pills { display: none; }
}
</style>
</head>
<body>

<!-- Header -->
<div class="header">
  <div class="header-inner">
    <a href="/" class="logo">
      <div class="logo-icon">E</div>
      <div class="logo-text">Equinox<small>Cross-Venue Intelligence</small></div>
    </a>
    <form class="search-form" method="GET" action="/">
      <input class="search-input" type="text" name="q" value="{{.SearchQuery}}" placeholder="Search markets...">
      <button class="search-btn" type="submit">Search</button>
    </form>
  </div>
</div>

<!-- Stats -->
<div class="stats-bar">
  <div class="stats-inner">
    {{range $venue, $count := .VenueCounts}}
    <div class="stat-item">
      <div class="stat-value">{{$count}}</div>
      <div class="stat-label">{{$venue}}</div>
    </div>
    <div class="stat-divider"></div>
    {{end}}
    <div class="stat-item">
      <span class="stat-badge stat-badge-match">{{.MatchCount}} Matches</span>
    </div>
    {{if .ProbableCount}}
    <div class="stat-item">
      <span class="stat-badge stat-badge-probable">{{.ProbableCount}} Probable</span>
    </div>
    {{end}}
    <div class="run-meta">
      <span class="material-icons-round" style="font-size:14px">schedule</span>
      {{.RunAt}}
      {{if .SearchQuery}} &middot; "{{.SearchQuery}}"{{end}}
      {{if .IsHomePage}} &middot; Trending{{end}}
    </div>
  </div>
</div>

<!-- Content -->
<div class="content">

{{if .Loading}}
<div class="loading-state">
  <div class="loading-spinner"></div>
  <div class="loading-text">Warming up pipeline...</div>
  <div class="loading-sub">Fetching markets from Polymarket and Kalshi, computing embeddings and matches.</div>
</div>
<script>setTimeout(function(){ location.reload(); }, 3000);</script>
{{else if not .Pairs}}
<div class="empty-state">
  <div class="empty-icon material-icons-round">search_off</div>
  <div class="empty-title">No equivalent pairs found</div>
  {{if .DiagnosisMessage}}
  <div class="diagnosis">
    <div class="diagnosis-box">
      <div class="diagnosis-label">Why no matches?</div>
      <div class="diagnosis-msg">{{.DiagnosisMessage}}</div>
    </div>
    {{if .NearMisses}}
    <div class="near-miss-title">Closest cross-venue candidates</div>
    {{range .NearMisses}}
    <div class="near-miss-card">
      <div class="near-miss-titles">
        <div style="flex:1">
          <div class="near-miss-venue">{{.VenueA}}</div>
          <div class="near-miss-t">{{.TitleA}}</div>
        </div>
        <div class="near-miss-vs">vs</div>
        <div style="flex:1">
          <div class="near-miss-venue">{{.VenueB}}</div>
          <div class="near-miss-t">{{.TitleB}}</div>
        </div>
      </div>
      <div class="near-miss-scores">
        <div class="near-miss-pill">Fuzzy <strong>{{score .FuzzyScore}}</strong></div>
        <div class="near-miss-pill">Embed <strong>{{score .EmbeddingScore}}</strong></div>
        <div class="near-miss-pill">Composite <strong>{{score .CompositeScore}}</strong></div>
        {{if gt .DatePenalty 0.0}}<div class="near-miss-pill">Date penalty <strong>{{score .DatePenalty}}</strong></div>{{end}}
      </div>
      <div class="near-miss-reason">{{.Reason}}</div>
    </div>
    {{end}}
    {{end}}
  </div>
  {{else}}
  <div class="empty-sub">Try a different search query, or adjust MATCH_THRESHOLD / MAX_DATE_DELTA_DAYS to widen the match window.</div>
  {{end}}
</div>
{{end}}

{{range $i, $p := .Pairs}}
<div class="pair-card" id="pair-{{$i}}">
  <!-- Header -->
  <div class="pair-head">
    <div class="pair-index">{{inc $i}}</div>
    <div class="conf-badge {{confClass $p.Confidence}}">
      <span class="material-icons-round">{{confIcon $p.Confidence}}</span>
      {{$p.Confidence}}
    </div>
    <div class="score-pills">
      <div class="score-pill">
        Fuzzy
        <div class="bar-mini"><div class="bar-mini-fill" style="width:{{scoreWidth $p.FuzzyScore}}"></div></div>
        <strong>{{score $p.FuzzyScore}}</strong>
      </div>
      <div class="score-pill">
        Embed
        <div class="bar-mini"><div class="bar-mini-fill" style="width:{{scoreWidth $p.EmbeddingScore}}"></div></div>
        <strong>{{score $p.EmbeddingScore}}</strong>
      </div>
      <div class="score-pill">
        Composite
        <div class="bar-mini"><div class="bar-mini-fill" style="width:{{scoreWidth $p.CompositeScore}}"></div></div>
        <strong>{{score $p.CompositeScore}}</strong>
      </div>
    </div>
  </div>

  <!-- Market comparison -->
  <div class="pair-body">
    <div class="market-col clickable-market"
         data-venue="{{$p.MarketA.Venue}}"
         data-market-id="{{$p.MarketA.VenueMarketID}}"
         data-title="{{$p.MarketA.Title}}"
         data-description="{{$p.MarketA.Description}}"
         data-category="{{$p.MarketA.Category}}"
         data-tags="{{$p.MarketA.Tags}}"
         data-status="{{$p.MarketA.Status}}"
         data-yes="{{printf "%.6f" $p.MarketA.YesPrice}}"
         data-liquidity="{{printf "%.2f" $p.MarketA.Liquidity}}"
         data-spread="{{printf "%.6f" $p.MarketA.Spread}}"
         data-resolution-date="{{$p.MarketA.ResolutionDate}}"
         data-created-at="{{$p.MarketA.CreatedAt}}"
         data-updated-at="{{$p.MarketA.UpdatedAt}}"
         data-volume24h="{{printf "%.2f" $p.MarketA.Volume24h}}"
         data-open-interest="{{printf "%.2f" $p.MarketA.OpenInterest}}"
         data-resolution-criteria="{{$p.MarketA.ResolutionRaw}}"
         data-venue-link="{{$p.MarketA.VenueLink}}"
         data-venue-search-link="{{$p.MarketA.VenueSearchLink}}"
         data-venue-search-link-alt="{{$p.MarketA.VenueSearchLinkAlt}}"
         data-payload="{{$p.MarketA.RawPayloadB64}}">
      <div class="venue-chip {{venueClass $p.MarketA.Venue}}">
        <span class="venue-chip-icon">{{venueIcon $p.MarketA.Venue}}</span>
        {{$p.MarketA.Venue}}
      </div>
      <div class="market-title">{{$p.MarketA.Title}}</div>
      <div class="price-row">
        <div>
          <div class="price-big yes-price">{{pct $p.MarketA.YesPrice}}</div>
          <div class="price-label">Yes Price</div>
        </div>
      </div>
      <div class="price-bar"><div class="price-bar-fill" style="width:{{pct $p.MarketA.YesPrice}}"></div></div>
      <div class="meta-grid">
        <div class="meta-cell">
          <div class="meta-cell-label">Liquidity</div>
          <div class="meta-cell-value">{{usd $p.MarketA.Liquidity}}</div>
        </div>
        <div class="meta-cell">
          <div class="meta-cell-label">Spread</div>
          <div class="meta-cell-value">{{if $p.MarketA.Spread}}{{pct $p.MarketA.Spread}}{{else}}N/A{{end}}</div>
        </div>
        {{if $p.MarketA.ResolutionDate}}
        <div class="meta-cell">
          <div class="meta-cell-label">Resolves</div>
          <div class="meta-cell-value">{{$p.MarketA.ResolutionDate}}</div>
        </div>
        {{end}}
        {{if $p.MarketA.Volume24h}}
        <div class="meta-cell">
          <div class="meta-cell-label">24h Vol</div>
          <div class="meta-cell-value">{{usd $p.MarketA.Volume24h}}</div>
        </div>
        {{end}}
      </div>
    </div>

    <div class="market-col clickable-market"
         data-venue="{{$p.MarketB.Venue}}"
         data-market-id="{{$p.MarketB.VenueMarketID}}"
         data-title="{{$p.MarketB.Title}}"
         data-description="{{$p.MarketB.Description}}"
         data-category="{{$p.MarketB.Category}}"
         data-tags="{{$p.MarketB.Tags}}"
         data-status="{{$p.MarketB.Status}}"
         data-yes="{{printf "%.6f" $p.MarketB.YesPrice}}"
         data-liquidity="{{printf "%.2f" $p.MarketB.Liquidity}}"
         data-spread="{{printf "%.6f" $p.MarketB.Spread}}"
         data-resolution-date="{{$p.MarketB.ResolutionDate}}"
         data-created-at="{{$p.MarketB.CreatedAt}}"
         data-updated-at="{{$p.MarketB.UpdatedAt}}"
         data-volume24h="{{printf "%.2f" $p.MarketB.Volume24h}}"
         data-open-interest="{{printf "%.2f" $p.MarketB.OpenInterest}}"
         data-resolution-criteria="{{$p.MarketB.ResolutionRaw}}"
         data-venue-link="{{$p.MarketB.VenueLink}}"
         data-venue-search-link="{{$p.MarketB.VenueSearchLink}}"
         data-venue-search-link-alt="{{$p.MarketB.VenueSearchLinkAlt}}"
         data-payload="{{$p.MarketB.RawPayloadB64}}">
      <div class="venue-chip {{venueClass $p.MarketB.Venue}}">
        <span class="venue-chip-icon">{{venueIcon $p.MarketB.Venue}}</span>
        {{$p.MarketB.Venue}}
      </div>
      <div class="market-title">{{$p.MarketB.Title}}</div>
      <div class="price-row">
        <div>
          <div class="price-big yes-price">{{pct $p.MarketB.YesPrice}}</div>
          <div class="price-label">Yes Price</div>
        </div>
      </div>
      <div class="price-bar"><div class="price-bar-fill" style="width:{{pct $p.MarketB.YesPrice}}"></div></div>
      <div class="meta-grid">
        <div class="meta-cell">
          <div class="meta-cell-label">Liquidity</div>
          <div class="meta-cell-value">{{usd $p.MarketB.Liquidity}}</div>
        </div>
        <div class="meta-cell">
          <div class="meta-cell-label">Spread</div>
          <div class="meta-cell-value">{{if $p.MarketB.Spread}}{{pct $p.MarketB.Spread}}{{else}}N/A{{end}}</div>
        </div>
        {{if $p.MarketB.ResolutionDate}}
        <div class="meta-cell">
          <div class="meta-cell-label">Resolves</div>
          <div class="meta-cell-value">{{$p.MarketB.ResolutionDate}}</div>
        </div>
        {{end}}
        {{if $p.MarketB.Volume24h}}
        <div class="meta-cell">
          <div class="meta-cell-label">24h Vol</div>
          <div class="meta-cell-value">{{usd $p.MarketB.Volume24h}}</div>
        </div>
        {{end}}
      </div>
    </div>
  </div>

  <!-- Routing footer -->
  <div class="pair-footer">
    <span class="route-label">Route to</span>
    <span class="material-icons-round route-arrow">arrow_forward</span>
    <span class="route-venue {{venueClass $p.SelectedVenue}}">{{$p.SelectedVenue}}</span>
    <button class="expand-btn" onclick="toggleExplain(this, 'explain-{{$i}}')">
      <span class="material-icons-round">expand_more</span>
      Details
    </button>
    <span class="route-reason" title="{{$p.Explanation}}">{{$p.Explanation}}</span>
  </div>

  <!-- Expandable explanation -->
  <div class="pair-explain" id="explain-{{$i}}">
    <div class="pair-explain-inner">{{$p.RoutingReason}}</div>
  </div>
</div>
{{end}}

</div>

<!-- Detail Modal -->
<div id="marketDetailModal" class="modal-overlay" aria-hidden="true">
  <div class="modal-bg" onclick="closeMarketModal()"></div>
  <div class="modal-container">
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
      <div class="modal-section">
        <div class="modal-section-title">Timestamps</div>
        <div class="modal-grid">
          <div class="modal-field"><div class="modal-field-label">Created</div><div class="modal-field-value" id="mdCreatedAt"></div></div>
          <div class="modal-field"><div class="modal-field-label">Updated</div><div class="modal-field-value" id="mdUpdatedAt"></div></div>
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
  // Expand/collapse explanation
  window.toggleExplain = function(btn, id) {
    var el = document.getElementById(id);
    if (!el) return;
    var open = el.classList.toggle("is-open");
    btn.classList.toggle("is-open", open);
  };

  // Modal
  var modal = document.getElementById("marketDetailModal");
  if (!modal) return;

  var fields = {};
  ["mdTitle","mdVenue","mdMarketId","mdStatus","mdDescription","mdTags",
   "mdCategory","mdResolutionDate","mdResolutionCriteria","mdYes",
   "mdLiquidity","mdSpread","mdCreatedAt","mdUpdatedAt","mdVolume",
   "mdOpenInterest","mdRawPayload","mdLinks"].forEach(function(id) {
    fields[id] = document.getElementById(id);
  });

  function safe(v) { return v ? String(v) : "--"; }

  function showMarketModal(card) {
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
    fields.mdCreatedAt.textContent = safe(d.createdAt);
    fields.mdUpdatedAt.textContent = safe(d.updatedAt);
    fields.mdVolume.textContent = safe(d.volume24h);
    fields.mdOpenInterest.textContent = safe(d.openInterest);
    fields.mdYes.textContent = safe(d.yes);
    fields.mdLiquidity.textContent = safe(d.liquidity);
    fields.mdSpread.textContent = safe(d.spread);

    var b64 = d.payload || "";
    if (b64) {
      try {
        var parsed = JSON.parse(atob(b64));
        fields.mdRawPayload.textContent = JSON.stringify(parsed, null, 2);
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
    var searchLink = safe(d.venueSearchLink);
    if (searchLink && searchLink !== "--") {
      links.innerHTML += '<a class="modal-link" href="' + searchLink + '" target="_blank"><span class="material-icons-round">search</span>Search on venue</a>';
    }
    var altLink = safe(d.venueSearchLinkAlt);
    if (altLink && altLink !== "--") {
      links.innerHTML += '<a class="modal-link" href="' + altLink + '" target="_blank"><span class="material-icons-round">travel_explore</span>Search fallback</a>';
    }

    modal.classList.add("is-open");
    modal.setAttribute("aria-hidden", "false");
    document.body.style.overflow = "hidden";
  }

  document.querySelectorAll(".clickable-market").forEach(function(card) {
    card.addEventListener("click", function() { showMarketModal(card); });
  });

  window.closeMarketModal = function() {
    modal.classList.remove("is-open");
    modal.setAttribute("aria-hidden", "true");
    document.body.style.overflow = "";
  };

  window.addEventListener("keydown", function(e) {
    if (e.key === "Escape") window.closeMarketModal();
  });

  // Animate score bars on scroll
  var observer = new IntersectionObserver(function(entries) {
    entries.forEach(function(entry) {
      if (entry.isIntersecting) {
        entry.target.style.opacity = "1";
        entry.target.style.transform = "translateY(0)";
      }
    });
  }, { threshold: 0.1 });

  document.querySelectorAll(".pair-card").forEach(function(card, i) {
    card.style.opacity = "0";
    card.style.transform = "translateY(16px)";
    card.style.transition = "opacity 400ms ease " + (i * 60) + "ms, transform 400ms ease " + (i * 60) + "ms";
    observer.observe(card);
  });
})();
</script>
</body>
</html>
`))
