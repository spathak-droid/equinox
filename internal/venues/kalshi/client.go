// Package kalshi implements the Venue interface for Kalshi using the v1 search API.
//
// Kalshi's v1 API (undocumented, powers kalshi.com) exposes a search endpoint
// at GET /v1/search/series that returns ranked series with nested markets and
// live prices. No auth required. This client uses that single endpoint for all
// operations: bulk fetch (by category), text search, and category fetch.
package kalshi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/equinox/internal/models"
	"github.com/equinox/internal/venues"
)

const (
	defaultSearchBase = "https://api.elections.kalshi.com/v1/search/series"
)

// ─── v1 search response types ───────────────────────────────────────────────

type searchResponse struct {
	TotalResultsCount int         `json:"total_results_count"`
	NextCursor        string      `json:"next_cursor"`
	CurrentPage       []seriesHit `json:"current_page"`
}

type productMetadata struct {
	CustomImageURL string `json:"custom_image_url"` // series-level thumbnail (best quality)
}

type seriesHit struct {
	SeriesTicker    string          `json:"series_ticker"`
	SeriesTitle     string          `json:"series_title"`
	EventTicker     string          `json:"event_ticker"`
	EventSubtitle   string          `json:"event_subtitle"`
	EventTitle      string          `json:"event_title"`
	Category        string          `json:"category"`
	Tags            []string        `json:"tags"`
	TotalVolume     int             `json:"total_volume"`
	ProductMetadata productMetadata `json:"product_metadata"`
	Markets         []v1Market      `json:"markets"`
}

type v1Market struct {
	Ticker             string `json:"ticker"`
	YesBid             int    `json:"yes_bid"`
	YesAsk             int    `json:"yes_ask"`
	LastPrice          int    `json:"last_price"`
	Volume             int    `json:"volume"`
	CloseTS            string `json:"close_ts"`
	YesSubtitle        string `json:"yes_subtitle"`
	NoSubtitle         string `json:"no_subtitle"`
	Result             string `json:"result"` // "yes", "no", or "" for active
	Score              int    `json:"score"`
	PriceDelta         int    `json:"price_delta"`
	IconURLLightMode   string `json:"icon_url_light_mode"`
	IconURLDarkMode    string `json:"icon_url_dark_mode"`
}

// ─── Client ─────────────────────────────────────────────────────────────────

// Client is a Kalshi API client. All operations go through the v1 search API.
type Client struct {
	http       *http.Client
	apiKey     string
	searchBase string
	maxMarkets int
}

// New returns a new Kalshi client. apiKey may be empty (v1 search needs no auth).
func New(apiKey string, timeout time.Duration, searchAPIURL string, maxMarkets ...int) *Client {
	limit := 0
	if len(maxMarkets) > 0 && maxMarkets[0] > 0 {
		limit = maxMarkets[0]
	}
	if searchAPIURL == "" {
		searchAPIURL = defaultSearchBase
	}
	return &Client{
		http:       &http.Client{Timeout: timeout},
		apiKey:     apiKey,
		searchBase: searchAPIURL,
		maxMarkets: limit,
	}
}

// ID implements venues.Venue.
func (c *Client) ID() models.VenueID {
	return models.VenueKalshi
}

// ─── FetchMarkets ───────────────────────────────────────────────────────────

// allCategories are the Kalshi categories we sweep to build a broad market set.
var allCategories = []string{
	"Politics", "Crypto", "Economics", "Sports",
	"Science and Technology", "Entertainment", "World", "Financials",
}

// FetchMarkets retrieves active markets by sweeping all categories via v1 search.
func (c *Client) FetchMarkets(ctx context.Context) ([]*venues.RawMarket, error) {
	perCat := 100
	if c.maxMarkets > 0 {
		perCat = c.maxMarkets / len(allCategories)
		if perCat < 10 {
			perCat = 10
		}
	}

	type result struct {
		markets []*venues.RawMarket
		err     error
	}

	var mu sync.Mutex
	var all []*venues.RawMarket
	seen := map[string]struct{}{}
	var wg sync.WaitGroup

	for _, cat := range allCategories {
		wg.Add(1)
		go func(category string) {
			defer wg.Done()

			markets, err := c.search(ctx, "", category, "volume", perCat)
			if err != nil {
				fmt.Printf("[kalshi] WARNING: category %q fetch failed: %v\n", category, err)
				return
			}

			mu.Lock()
			for _, m := range markets {
				if _, ok := seen[m.VenueMarketID]; ok {
					continue
				}
				seen[m.VenueMarketID] = struct{}{}
				all = append(all, m)
			}
			mu.Unlock()
		}(cat)
	}

	wg.Wait()

	// Apply global cap
	if c.maxMarkets > 0 && len(all) > c.maxMarkets {
		all = all[:c.maxMarkets]
	}

	fmt.Printf("[kalshi] Fetched %d markets across %d categories\n", len(all), len(allCategories))
	return all, nil
}

// ─── FetchMarketsByQuery ────────────────────────────────────────────────────

// FetchMarketsByQuery searches for markets matching a text query.
func (c *Client) FetchMarketsByQuery(ctx context.Context, query string) ([]*venues.RawMarket, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	return c.search(ctx, q, "", "querymatch", 100)
}

// ─── FetchMarketsByCategory ─────────────────────────────────────────────────

// kalshiCategories maps normalized category names to Kalshi API category values.
var kalshiCategories = map[string]string{
	"politics":      "Politics",
	"crypto":        "Crypto",
	"economics":     "Economics",
	"sports":        "Sports",
	"science":       "Science and Technology",
	"technology":    "Science and Technology",
	"entertainment": "Entertainment",
	"world":         "World",
	"finance":       "Financials",
}

// FetchMarketsByCategory returns active markets for a given category.
func (c *Client) FetchMarketsByCategory(ctx context.Context, category string) ([]*venues.RawMarket, error) {
	kalshiCat, ok := kalshiCategories[strings.ToLower(category)]
	if !ok {
		return nil, fmt.Errorf("kalshi: unknown category %q", category)
	}
	return c.search(ctx, "", kalshiCat, "volume", 50)
}

// FetchMarketsByCategoryWithLimit returns active markets for a category with a limit.
func (c *Client) FetchMarketsByCategoryWithLimit(ctx context.Context, category string, limit int) ([]*venues.RawMarket, error) {
	kalshiCat, ok := kalshiCategories[strings.ToLower(category)]
	if !ok {
		return nil, fmt.Errorf("kalshi: unknown category %q", category)
	}
	if limit <= 0 {
		limit = 50
	}
	return c.search(ctx, "", kalshiCat, "volume", limit)
}

// ─── Core search ────────────────────────────────────────────────────────────

// search calls GET /v1/search/series with the given parameters and returns
// flattened RawMarkets. This is the single entry point for all Kalshi data.
func (c *Client) search(ctx context.Context, query, category, orderBy string, limit int) ([]*venues.RawMarket, error) {
	params := url.Values{}
	if query != "" {
		params.Set("query", query)
	}
	if category != "" {
		params.Set("category", category)
	}
	if orderBy != "" {
		params.Set("order_by", orderBy)
	}
	if limit > 100 {
		limit = 100 // v1 API caps around 100
	}
	params.Set("page_size", fmt.Sprintf("%d", limit))

	u := c.searchBase + "?" + params.Encode()
	fmt.Printf("[kalshi] GET %s\n", u)

	body, err := c.doGet(ctx, u)
	if err != nil {
		return nil, err
	}

	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("kalshi search unmarshal: %w", err)
	}

	markets := flattenHits(resp.CurrentPage)
	fmt.Printf("[kalshi] q=%q cat=%q → %d series, %d markets\n",
		query, category, len(resp.CurrentPage), len(markets))
	for i, m := range markets {
		if i >= 5 {
			fmt.Printf("[kalshi]   ... and %d more\n", len(markets)-5)
			break
		}
		// Parse back the payload to show the title
		var p struct {
			EventTitle string `json:"event_title"`
			Subtitle   string `json:"subtitle"`
			YesBid     int    `json:"yes_bid"`
			YesAsk     int    `json:"yes_ask"`
		}
		json.Unmarshal(m.Payload, &p)
		title := p.EventTitle
		if p.Subtitle != "" {
			title += " — " + p.Subtitle
		}
		fmt.Printf("[kalshi]   [%d] %s (bid=%d ask=%d)\n", i+1, title, p.YesBid, p.YesAsk)
	}
	return markets, nil
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// flattenHits converts v1 search series hits into flat RawMarket slices.
// Each market gets series/event context injected so the normalizer can build
// a full CanonicalMarket.
func flattenHits(hits []seriesHit) []*venues.RawMarket {
	var out []*venues.RawMarket
	seen := map[string]struct{}{}

	const maxMarketsPerSeries = 10
	for _, hit := range hits {
		hitCount := 0
		for _, mkt := range hit.Markets {
			if _, ok := seen[mkt.Ticker]; ok {
				continue
			}
			// Skip resolved markets
			if mkt.Result == "yes" || mkt.Result == "no" {
				continue
			}
			if hitCount >= maxMarketsPerSeries {
				break
			}
			seen[mkt.Ticker] = struct{}{}

			// Build a payload the normalizer's kalshiRaw struct can parse.
			// Prefer the series-level custom image (meaningful thumbnail).
			// Fall back to the market-level icon only if custom is absent.
			imageURL := hit.ProductMetadata.CustomImageURL
			if imageURL == "" {
				imageURL = mkt.IconURLLightMode
			}

			payload := map[string]interface{}{
				"ticker":               mkt.Ticker,
				"event_ticker":         hit.EventTicker,
				"series_ticker":        hit.SeriesTicker,
				"event_title":          hit.EventTitle,
				"title":                hit.EventTitle,
				"subtitle":             mkt.YesSubtitle,
				"status":               "active",
				"close_time":           mkt.CloseTS,
				"yes_bid":              mkt.YesBid,
				"yes_ask":              mkt.YesAsk,
				"no_bid":               100 - mkt.YesAsk,
				"no_ask":               100 - mkt.YesBid,
				"volume":               mkt.Volume,
				"volume_24h":           0,
				"open_interest":        0,
				"liquidity":            0,
				"image_url_light_mode": imageURL,
				"image_url_dark_mode":  mkt.IconURLDarkMode,
			}

			b, err := json.Marshal(payload)
			if err != nil {
				continue
			}

			out = append(out, &venues.RawMarket{
				VenueID:       models.VenueKalshi,
				VenueMarketID: mkt.Ticker,
				FetchCategory: strings.ToLower(hit.Category),
				Payload:       b,
			})
			hitCount++
		}
	}
	return out
}

// doGet performs an HTTP GET with retry on 429.
func (c *Client) doGet(ctx context.Context, rawURL string) ([]byte, error) {
	const maxRetries = 3
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "equinox-prototype/1.0")

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HTTP GET %s: %w", rawURL, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt == maxRetries {
				return nil, fmt.Errorf("429 from %s after %d retries", rawURL, maxRetries)
			}
			fmt.Printf("[kalshi] Rate limited, retrying in %ds...\n", 1<<uint(attempt+1))
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d from %s: %s", resp.StatusCode, rawURL, string(body))
		}

		return body, nil
	}
	return nil, fmt.Errorf("max retries exceeded for %s", rawURL)
}
