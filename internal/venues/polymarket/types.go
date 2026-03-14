package polymarket

import "encoding/json"

// polymarketMarket is the raw shape returned by Polymarket's Gamma API.
// Only fields we use downstream are defined here; the full payload is retained as RawPayload.
type polymarketMarket struct {
	ID            string  `json:"id"`
	Question      string  `json:"question"`
	Description   string  `json:"description"`
	EndDateISO    string  `json:"endDateIso"`
	Active        bool    `json:"active"`
	Closed        bool    `json:"closed"`
	OutcomePrices string  `json:"outcomePrices"` // JSON array string e.g. "[\"0.62\", \"0.38\"]"
	Volume        string  `json:"volume"`        // API returns string
	Liquidity     string  `json:"liquidity"`     // API returns string
	Category      string  `json:"category"`
	Tags          []struct {
		Label string `json:"label"`
	} `json:"tags"`
}

// ─── Public search response types ───────────────────────────────────────────

// publicSearchResponse matches the actual Polymarket public-search API response.
type publicSearchResponse struct {
	Events []publicSearchEvent `json:"events"`
}

type publicSearchEvent struct {
	Title   string               `json:"title"`
	Slug    string               `json:"slug"`
	EndDate string               `json:"endDate"`
	Image   string               `json:"image"`
	Active  bool                 `json:"active"`
	Closed  bool                 `json:"closed"`
	Markets []publicSearchMarket `json:"markets"`
}

type publicSearchMarket struct {
	Question       string   `json:"question"`
	Slug           string   `json:"slug"`
	Active         bool     `json:"active"`
	Closed         bool     `json:"closed"`
	BestBid        float64  `json:"bestBid"`
	BestAsk        float64  `json:"bestAsk"`
	LastTradePrice float64  `json:"lastTradePrice"`
	Spread         float64  `json:"spread"`
	OutcomePrices  []string `json:"outcomePrices"` // actual array: ["0.935","0.065"]
	Outcomes       []string `json:"outcomes"`      // ["Yes","No"]
	GroupItemTitle string   `json:"groupItemTitle"`
}

// searchEntry pairs a public-search market with its parent event metadata.
type searchEntry struct {
	mkt publicSearchMarket
	ev  publicSearchEvent
}

// fullMarketData holds fields we enrich from /markets?slug=...
type fullMarketData struct {
	LiquidityNum float64 `json:"liquidityNum"`
	Volume24hr   float64 `json:"volume24hr"`
	ClobTokenIDs string  `json:"clobTokenIds"`
	Description  string  `json:"description"`
	Category     string  `json:"category"`
	Tags         []struct {
		Label string `json:"label"`
	} `json:"tags"`
}

// ─── Category mappings ──────────────────────────────────────────────────────

// polymarketTagSlugs maps normalized category names to Polymarket tag slugs.
var polymarketTagSlugs = map[string]string{
	"politics":      "politics",
	"crypto":        "crypto",
	"economics":     "economics",
	"sports":        "sports",
	"science":       "science",
	"technology":    "technology",
	"entertainment": "entertainment",
	"world":         "world",
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// injectImageIntoPayload merges an event-level image URL into a market JSON payload.
// If imageURL is empty or merging fails, the original payload is returned unchanged.
func injectImageIntoPayload(payload json.RawMessage, imageURL string) json.RawMessage {
	if imageURL == "" {
		return payload
	}
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return payload
	}
	m["image"] = imageURL
	b, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return b
}
