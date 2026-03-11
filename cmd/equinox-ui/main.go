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
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
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

// PageData is passed to the HTML template.
type PageData struct {
	SearchQuery      string
	Pairs            []PairView
	VenueCounts      map[models.VenueID]int
	MatchCount       int
	ProbableCount    int
	NearMisses       []NearMissView
	DiagnosisMessage string
	HasQuery         bool
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
	if err := godotenv.Load(); err != nil {
		log.Fatal("Error loading .env file")
	}
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

		var data *PageData
		if query == "" {
			data = &PageData{VenueCounts: map[models.VenueID]int{}}
		} else {
			fmt.Printf("[equinox-ui] Running search pipeline q=%q\n", query)
			data, err = runSearchPipeline(cfg, kalshiClient, polyClient, query)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := pageTmpl.Execute(w, data); err != nil {
			fmt.Printf("[equinox-ui] ERROR: rendering template: %v\n", err)
		}
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}

// runSearchPipeline searches both venues for a query, then cross-matches
// Polymarket results against Kalshi results directly (no brute-force n×n).
func runSearchPipeline(cfg *config.Config, kalshiClient *kalshi.Client, polyClient *polymarket.Client, query string) (*PageData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

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

	norm := normalizer.New(cfg)
	polyMarkets, _ := norm.Normalize(ctx, polyRaw)
	kalshiMarkets, _ := norm.Normalize(ctx, kalshiRaw)

	venueCounts := map[models.VenueID]int{
		models.VenuePolymarket: len(polyMarkets),
		models.VenueKalshi:     len(kalshiMarkets),
	}
	fmt.Printf("[equinox-ui] Search results: poly=%d kalshi=%d\n", len(polyMarkets), len(kalshiMarkets))

	if len(polyMarkets) == 0 || len(kalshiMarkets) == 0 {
		missingVenue := "Polymarket"
		if len(kalshiMarkets) == 0 {
			missingVenue = "Kalshi"
		}
		fmt.Printf("[equinox-ui] WARNING: %s returned 0 markets for %q\n", missingVenue, query)
	}

	// Build search results: both directions, no source/candidate distinction
	var searchResults []matcher.SearchResult
	for _, pm := range polyMarkets {
		searchResults = append(searchResults, matcher.SearchResult{
			Source:     pm,
			Candidates: kalshiMarkets,
		})
	}
	for _, km := range kalshiMarkets {
		searchResults = append(searchResults, matcher.SearchResult{
			Source:     km,
			Candidates: polyMarkets,
		})
	}

	// Score using the search matcher (not brute-force)
	m := matcher.New(cfg)
	pairs := m.FindEquivalentPairsFromSearch(ctx, searchResults)

	var allMarkets []*models.CanonicalMarket
	allMarkets = append(allMarkets, polyMarkets...)
	allMarkets = append(allMarkets, kalshiMarkets...)

	return buildPageData(cfg, ctx, m, allMarkets, pairs, venueCounts, query)
}

// buildPageData takes pre-computed match pairs and builds the PageData for the template.
func buildPageData(cfg *config.Config, ctx context.Context, m *matcher.Matcher, allMarkets []*models.CanonicalMarket, pairs []*matcher.MatchResult, venueCounts map[models.VenueID]int, query string) (*PageData, error) {
	if len(pairs) > maxDisplayPairs {
		pairs = pairs[:maxDisplayPairs]
	}

	var nearMisses []NearMissView
	var diagnosisMsg string
	if len(pairs) == 0 {
		rejected := m.TopRejectedPairs(allMarkets, 5)
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
		NearMisses:       nearMisses,
		DiagnosisMessage: diagnosisMsg,
		HasQuery:         true,
	}, nil
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
.near-miss-title { font-size: 0.68rem; font-weight: 700; letter-spacing: 0.05em; text-transform: uppercase; color: var(--text-muted); margin-bottom: 8px; }
.near-miss-card { background: var(--bg-elevated); border: 1px solid var(--border); border-radius: var(--radius-sm); padding: 10px 12px; margin-bottom: 6px; }
.near-miss-titles { display: flex; gap: 6px; align-items: flex-start; margin-bottom: 6px; }
.near-miss-vs { color: var(--text-muted); font-size: 0.68rem; font-weight: 600; flex-shrink: 0; padding-top: 1px; }
.near-miss-t { font-size: 0.78rem; color: var(--text-primary); flex: 1; line-height: 1.3; }
.near-miss-venue { font-size: 0.65rem; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.03em; }
.near-miss-scores { display: flex; gap: 8px; flex-wrap: wrap; }
.near-miss-pill { font-size: 0.68rem; color: var(--text-muted); background: var(--bg-card); padding: 1px 6px; border-radius: 999px; }
.near-miss-pill strong { color: var(--text-secondary); font-weight: 600; }
.near-miss-reason { font-size: 0.7rem; color: var(--text-muted); margin-top: 4px; font-style: italic; }

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
.pair-explain.is-open { max-height: 500px; }
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

@media (max-width: 768px) {
  .pair-body { grid-template-columns: 1fr; }
  .market-col:first-child { border-right: none; border-bottom: 1px solid var(--border); }
  .header-inner { padding: 8px 12px; }
  .content { padding: 12px; }
  .modal-grid { grid-template-columns: 1fr; }
  .score-pills { display: none; }
  .hero-title { font-size: 1.8rem; }
}
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
    <input class="hero-input" type="text" name="q" placeholder="e.g. Bitcoin, Trump, Fed rate..." autofocus>
    <button class="hero-btn" type="submit">Search</button>
  </form>
  <div class="hero-hints">
    <a class="hero-hint" href="/?q=Bitcoin">Bitcoin</a>
    <a class="hero-hint" href="/?q=Trump">Trump</a>
    <a class="hero-hint" href="/?q=Federal+Reserve+rate">Fed rate</a>
    <a class="hero-hint" href="/?q=2024+election">2024 election</a>
    <a class="hero-hint" href="/?q=recession">Recession</a>
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
        <span class="mkt-price">{{pct $p.MarketA.YesPrice}}</span>
        <span class="mkt-stat">Liq <span>{{usd $p.MarketA.Liquidity}}</span></span>
        <span class="mkt-stat">Spread <span>{{if $p.MarketA.Spread}}{{pct $p.MarketA.Spread}}{{else}}--{{end}}</span></span>
        {{if $p.MarketA.ResolutionDate}}<span class="mkt-stat">Res <span>{{$p.MarketA.ResolutionDate}}</span></span>{{end}}
        {{if $p.MarketA.Volume24h}}<span class="mkt-stat">24h <span>{{usd $p.MarketA.Volume24h}}</span></span>{{end}}
      </div>
    </div>
    <div class="market-col clickable-market"
         data-venue="{{$p.MarketB.Venue}}" data-market-id="{{$p.MarketB.VenueMarketID}}" data-title="{{$p.MarketB.Title}}"
         data-description="{{$p.MarketB.Description}}" data-category="{{$p.MarketB.Category}}" data-tags="{{$p.MarketB.Tags}}"
         data-status="{{$p.MarketB.Status}}" data-yes="{{printf "%.6f" $p.MarketB.YesPrice}}"
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
        <span class="mkt-price">{{pct $p.MarketB.YesPrice}}</span>
        <span class="mkt-stat">Liq <span>{{usd $p.MarketB.Liquidity}}</span></span>
        <span class="mkt-stat">Spread <span>{{if $p.MarketB.Spread}}{{pct $p.MarketB.Spread}}{{else}}--{{end}}</span></span>
        {{if $p.MarketB.ResolutionDate}}<span class="mkt-stat">Res <span>{{$p.MarketB.ResolutionDate}}</span></span>{{end}}
        {{if $p.MarketB.Volume24h}}<span class="mkt-stat">24h <span>{{usd $p.MarketB.Volume24h}}</span></span>{{end}}
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
   "mdLiquidity","mdSpread","mdCreatedAt","mdUpdatedAt","mdVolume",
   "mdOpenInterest","mdRawPayload","mdLinks","mdImage","mdImageBanner"].forEach(function(id) {
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
})();
</script>
</body>
</html>
`))
