package main

import (
	"sync"
	"time"

	"github.com/equinox/internal/models"
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

// IndexStats holds index metadata for display.
type IndexStats struct {
	Total      int       `json:"total"`
	Polymarket int       `json:"polymarket"`
	Kalshi     int       `json:"kalshi"`
	LastUpdate time.Time `json:"last_update"`
}

// PageData is passed to the HTML template.
type PageData struct {
	SearchQuery      string
	Pairs            []PairView
	UnpairedMarkets  []MarketView // markets that didn't match cross-venue
	VenueCounts      map[models.VenueID]int
	MatchCount       int
	ProbableCount    int
	DiagnosisMessage string
	HasQuery         bool
	DeepSearch       bool
	BrowseMode        bool
	IndexSearch       bool   // results came from index, show "Search Live" button
	LiveSearchPending bool   // no index available, auto-trigger SSE on page load
	IndexStats        *IndexStats
	IndexLoading      bool   // DB still initializing, show loader
}

// NewsArticleView is a single news article ready for rendering.
type NewsArticleView struct {
	Title  string `json:"title"`
	Source string `json:"source"`
	URL    string `json:"url"`
	Age    string `json:"age"`
}

// PairView is a single matched pair ready for rendering.
type PairView struct {
	MarketA        MarketView        `json:"market_a"`
	MarketB        MarketView        `json:"market_b"`
	Confidence     string            `json:"confidence"`
	FuzzyScore      float64           `json:"fuzzy_score"`
	EmbeddingScore  float64           `json:"embedding_score"`
	CompositeScore  float64           `json:"composite_score"`
	ConfidenceScore float64           `json:"confidence_score"`
	Explanation    string            `json:"explanation"`
	SelectedVenue  string            `json:"selected_venue"`
	RoutingReason  string            `json:"routing_reason"`
	NewsQuery      string            `json:"news_query,omitempty"`
	NewsArticles   []NewsArticleView `json:"news_articles,omitempty"`
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
	NoPrice            float64 `json:"no_price"`
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
