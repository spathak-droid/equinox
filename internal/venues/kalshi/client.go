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
	v2MarketsURL      = "https://api.elections.kalshi.com/trade-api/v2/markets"
	maxRespSize       = 10 * 1024 * 1024 // 10MB limit for venue API responses
)

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

// FetchMarkets retrieves active markets by sweeping all categories via v1 search.
func (c *Client) FetchMarkets(ctx context.Context) ([]*venues.RawMarket, error) {
	perCat := 100
	if c.maxMarkets > 0 {
		perCat = c.maxMarkets / len(allCategories)
		if perCat < 10 {
			perCat = 10
		}
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
	markets, _, err := c.searchPaged(ctx, q, "", "querymatch", 100, "")
	return markets, err
}

// FetchMarketsByQueryPaged fetches one page of query results. Pass cursor=""
// for the first page; subsequent pages use the returned nextCursor.
func (c *Client) FetchMarketsByQueryPaged(ctx context.Context, query, cursor string, pageSize int) ([]*venues.RawMarket, string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, "", nil
	}
	return c.searchPaged(ctx, q, "", "querymatch", pageSize, cursor)
}

// ─── FetchMarketsByCategory ─────────────────────────────────────────────────

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

// ─── FetchAllOpenMarkets (v2 bulk indexing) ─────────────────────────────────

// FetchAllOpenMarkets retrieves ALL open markets from Kalshi using the v2 REST API
// with cursor-based pagination. This is used by the indexer for bulk data collection.
// Unlike the v1 search API, v2 supports limit=1000 and returns structured market data.
func (c *Client) FetchAllOpenMarkets(ctx context.Context) ([]*venues.RawMarket, error) {
	var all []*venues.RawMarket
	seen := map[string]struct{}{}
	cursor := ""
	pageNum := 0

	for {
		pageNum++
		params := url.Values{}
		params.Set("status", "open")
		params.Set("limit", "1000")
		if cursor != "" {
			params.Set("cursor", cursor)
		}

		u := v2MarketsURL + "?" + params.Encode()
		if pageNum <= 3 || pageNum%10 == 0 {
			fmt.Printf("[kalshi/v2] page %d: GET %s\n", pageNum, u)
		}

		body, err := c.doGet(ctx, u)
		if err != nil {
			if len(all) > 0 {
				fmt.Printf("[kalshi/v2] WARNING: pagination stopped after %d markets: %v\n", len(all), err)
				break
			}
			return nil, fmt.Errorf("kalshi v2 markets: %w", err)
		}

		var resp v2MarketsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			if len(all) > 0 {
				fmt.Printf("[kalshi/v2] WARNING: unmarshal failed after %d markets, returning partial results: %v\n", len(all), err)
				break
			}
			return nil, fmt.Errorf("kalshi v2 unmarshal: %w", err)
		}

		if len(resp.Markets) == 0 {
			break
		}

		for _, mkt := range resp.Markets {
			if _, ok := seen[mkt.Ticker]; ok {
				continue
			}
			if mkt.Result == "yes" || mkt.Result == "no" {
				continue
			}
			seen[mkt.Ticker] = struct{}{}

			subtitle := mkt.Subtitle
			if subtitle == "" {
				subtitle = mkt.YesSubTitle
			}

			yesBid := dollarsToCents(mkt.YesBidDollars)
			yesAsk := dollarsToCents(mkt.YesAskDollars)
			noBid := dollarsToCents(mkt.NoBidDollars)
			noAsk := dollarsToCents(mkt.NoAskDollars)
			volume := parseFloatStr(mkt.VolumeFP)
			volume24h := parseFloatStr(mkt.Volume24hFP)
			openInterest := parseFloatStr(mkt.OpenInterestFP)
			liquidity := parseFloatStr(mkt.LiquidityDollars)

			payload := map[string]interface{}{
				"ticker":          mkt.Ticker,
				"event_ticker":    mkt.EventTicker,
				"title":           mkt.Title,
				"event_title":     mkt.Title,
				"subtitle":        subtitle,
				"status":          mkt.Status,
				"close_time":      mkt.CloseTime,
				"yes_bid":         yesBid,
				"yes_ask":         yesAsk,
				"no_bid":          noBid,
				"no_ask":          noAsk,
				"volume":          volume,
				"volume_24h":      volume24h,
				"open_interest":   openInterest,
				"liquidity":       liquidity,
				"rules_primary":   mkt.RulesPrimary,
				"rules_secondary": mkt.RulesSecondary,
			}

			b, err := json.Marshal(payload)
			if err != nil {
				continue
			}

			cat := strings.ToLower(mkt.Category)
			if cat == "" {
				cat = "other"
			}

			all = append(all, &venues.RawMarket{
				VenueID:       models.VenueKalshi,
				VenueMarketID: mkt.Ticker,
				FetchCategory: cat,
				Payload:       b,
			})
		}

		if pageNum%5 == 0 {
			fmt.Printf("[kalshi/v2] indexed %d markets so far...\n", len(all))
		}

		if resp.Cursor == "" {
			break
		}
		cursor = resp.Cursor
	}

	fmt.Printf("[kalshi/v2] Fetched %d open markets total\n", len(all))
	return all, nil
}

// ─── Enrichment ─────────────────────────────────────────────────────────────

// EnrichWithV1Data merges image URLs and series_tickers from v1 search data into
// v2 markets. Call this after FetchAllOpenMarkets to add images for display.
func (c *Client) EnrichWithV1Data(ctx context.Context, v2Markets []*venues.RawMarket) {
	type enrichment struct {
		SeriesTicker string
		EventTitle   string
		ImageURL     string
	}
	enrichMap := make(map[string]enrichment)

	for _, cat := range allCategories {
		markets, err := c.search(ctx, "", cat, "volume", 100)
		if err != nil {
			continue
		}
		for _, m := range markets {
			var p struct {
				EventTicker  string `json:"event_ticker"`
				SeriesTicker string `json:"series_ticker"`
				EventTitle   string `json:"event_title"`
				ImageURL     string `json:"image_url_light_mode"`
			}
			if err := json.Unmarshal(m.Payload, &p); err != nil {
				fmt.Printf("[kalshi] warning: unmarshal payload for enrichment: %v\n", err)
				continue
			}
			if p.EventTicker != "" {
				enrichMap[p.EventTicker] = enrichment{
					SeriesTicker: p.SeriesTicker,
					EventTitle:   p.EventTitle,
					ImageURL:     p.ImageURL,
				}
			}
		}
	}

	if len(enrichMap) == 0 {
		return
	}

	enriched := 0
	for i, m := range v2Markets {
		var p map[string]interface{}
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			continue
		}
		et, _ := p["event_ticker"].(string)
		if e, ok := enrichMap[et]; ok {
			changed := false
			if e.SeriesTicker != "" {
				p["series_ticker"] = e.SeriesTicker
				changed = true
			}
			if e.EventTitle != "" {
				p["event_title"] = e.EventTitle
				changed = true
			}
			if e.ImageURL != "" {
				p["image_url_light_mode"] = e.ImageURL
				changed = true
			}
			if changed {
				if b, err := json.Marshal(p); err == nil {
					v2Markets[i].Payload = b
					enriched++
				}
			}
		}
	}
	fmt.Printf("[kalshi/v2] Enriched %d markets with images/series_ticker/event_title from v1 (%d entries in map)\n", enriched, len(enrichMap))
}

// FetchEventImages looks up image URLs for a set of event tickers using the v1
// search API. Returns a map of event_ticker to image_url.
func (c *Client) FetchEventImages(ctx context.Context, eventTickers []string) map[string]string {
	if len(eventTickers) == 0 {
		return nil
	}

	result := make(map[string]string)

	searched := map[string]bool{}
	for _, et := range eventTickers {
		series := et
		if idx := strings.LastIndex(et, "-"); idx > 0 {
			series = et[:idx]
		}
		if searched[series] {
			continue
		}
		searched[series] = true

		body, err := c.doGet(ctx, c.searchBase+"?query="+url.QueryEscape(series)+"&page_size=20")
		if err != nil {
			continue
		}
		var resp searchResponse
		if json.Unmarshal(body, &resp) != nil {
			continue
		}
		for _, hit := range resp.CurrentPage {
			img := hit.ProductMetadata.CustomImageURL
			if img == "" {
				for _, mkt := range hit.Markets {
					if mkt.IconURLLightMode != "" {
						img = mkt.IconURLLightMode
						break
					}
				}
			}
			if img != "" && hit.EventTicker != "" {
				result[hit.EventTicker] = img
			}
		}
	}

	return result
}

// ─── Core search ────────────────────────────────────────────────────────────

// search calls GET /v1/search/series with the given parameters and returns
// flattened RawMarkets. This is the single entry point for all Kalshi data.
func (c *Client) search(ctx context.Context, query, category, orderBy string, limit int) ([]*venues.RawMarket, error) {
	markets, _, err := c.searchPaged(ctx, query, category, orderBy, limit, "")
	return markets, err
}

func (c *Client) searchPaged(ctx context.Context, query, category, orderBy string, limit int, cursor string) ([]*venues.RawMarket, string, error) {
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
		limit = 100
	}
	params.Set("page_size", fmt.Sprintf("%d", limit))
	if cursor != "" {
		params.Set("cursor", cursor)
	}

	u := c.searchBase + "?" + params.Encode()
	fmt.Printf("[kalshi] GET %s\n", u)

	body, err := c.doGet(ctx, u)
	if err != nil {
		return nil, "", err
	}

	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", fmt.Errorf("kalshi search unmarshal: %w", err)
	}

	markets := flattenHits(resp.CurrentPage)
	fmt.Printf("[kalshi] q=%q cat=%q → %d series, %d markets\n",
		query, category, len(resp.CurrentPage), len(markets))
	for i, m := range markets {
		if i >= 5 {
			fmt.Printf("[kalshi]   ... and %d more\n", len(markets)-5)
			break
		}
		var p struct {
			EventTitle string `json:"event_title"`
			Subtitle   string `json:"subtitle"`
			YesBid     int    `json:"yes_bid"`
			YesAsk     int    `json:"yes_ask"`
		}
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			fmt.Printf("[kalshi] warning: unmarshal payload for debug logging: %v\n", err)
		}
		title := p.EventTitle
		if p.Subtitle != "" {
			title += " — " + p.Subtitle
		}
		fmt.Printf("[kalshi]   [%d] %s (bid=%d ask=%d)\n", i+1, title, p.YesBid, p.YesAsk)
	}
	return markets, resp.NextCursor, nil
}

// ─── HTTP ───────────────────────────────────────────────────────────────────

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
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespSize))
		if err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("[kalshi] reading response body: %w", err)
		}
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
