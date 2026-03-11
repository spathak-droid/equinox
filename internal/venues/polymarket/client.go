// Package polymarket implements the Venue interface for Polymarket's public Gamma API.
// API docs: https://docs.polymarket.com/
// Base URL:  https://gamma-api.polymarket.com
//
// Polymarket markets are structured as "events" containing one or more binary outcome markets.
// We treat each binary market (outcome) as an individual CanonicalMarket.
//
// Auth: Polymarket's Gamma API is public and requires no authentication for reads.
package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/equinox/internal/models"
	"github.com/equinox/internal/venues"
)

const (
	baseURL  = "https://gamma-api.polymarket.com"
	pageSize = 100
)

// Client is a Polymarket Gamma API client.
type Client struct {
	http         *http.Client
	baseURL      string
	searchAPIURL string // public-search endpoint URL
	maxMarkets   int
}

// New returns a new Polymarket client.
func New(timeout time.Duration, searchAPIURL string, maxMarkets ...int) *Client {
	limit := 0 // 0 = unlimited
	if len(maxMarkets) > 0 && maxMarkets[0] > 0 {
		limit = maxMarkets[0]
	}
	if searchAPIURL == "" {
		searchAPIURL = baseURL + "/public-search"
	}
	return &Client{
		http:         &http.Client{Timeout: timeout},
		baseURL:      baseURL,
		searchAPIURL: searchAPIURL,
		maxMarkets:   limit,
	}
}

// ID implements venues.Venue.
func (c *Client) ID() models.VenueID {
	return models.VenuePolymarket
}

// FetchMarkets retrieves all active markets from Polymarket and paginates automatically.
// Each market is returned as a RawMarket with the verbatim JSON payload.
func (c *Client) FetchMarkets(ctx context.Context) ([]*venues.RawMarket, error) {
	return c.fetchMarketsWithFilter(ctx, nil)
}

// FetchMarketsByQuery retrieves markets using Polymarket's public-search endpoint.
// No fallback — if public-search returns nothing, we return nothing.
func (c *Client) FetchMarketsByQuery(ctx context.Context, query string) ([]*venues.RawMarket, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return []*venues.RawMarket{}, nil
	}

	u := fmt.Sprintf("%s?q=%s&keep_closed_markets=0&optimized=true&cache=true", c.searchAPIURL, url.QueryEscape(q))
	fmt.Printf("[polymarket] GET %s\n", u)
	return c.fetchPublicSearch(ctx, u)
}

// polymarketTagSlugs maps normalized category names to Polymarket tag slugs.
var polymarketTagSlugs = map[string]string{
	"politics":    "politics",
	"crypto":      "crypto",
	"economics":   "economics",
	"sports":      "sports",
	"science":     "science",
	"technology":  "technology",
	"entertainment": "entertainment",
	"world":       "world",
}

// FetchMarketsByCategory returns active markets for a given category using the
// /events?tag_slug=... endpoint. Each event may contain multiple markets;
// they are flattened into individual RawMarket entries.
func (c *Client) FetchMarketsByCategory(ctx context.Context, category string) ([]*venues.RawMarket, error) {
	tagSlug, ok := polymarketTagSlugs[strings.ToLower(category)]
	if !ok {
		return nil, fmt.Errorf("polymarket: unknown category %q", category)
	}

	limit := 50
	if c.maxMarkets > 0 && c.maxMarkets < limit {
		limit = c.maxMarkets
	}

	u := fmt.Sprintf("%s/events?tag_slug=%s&active=true&closed=false&limit=%d&order=volume24hr&ascending=false",
		c.baseURL, url.QueryEscape(tagSlug), limit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, u)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	// Events endpoint returns an array of events, each with nested markets
	var events []struct {
		Markets []json.RawMessage `json:"markets"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("unmarshalling events: %w", err)
	}

	var result []*venues.RawMarket
	seen := map[string]struct{}{}
	for _, ev := range events {
		for _, item := range ev.Markets {
			var m polymarketMarket
			if err := json.Unmarshal(item, &m); err != nil {
				continue
			}
			if !m.Active || m.Closed || m.ID == "" {
				continue
			}
			if _, ok := seen[m.ID]; ok {
				continue
			}
			seen[m.ID] = struct{}{}
			result = append(result, &venues.RawMarket{
				VenueID:       models.VenuePolymarket,
				VenueMarketID: m.ID,
				Payload:       item,
			})
			if c.maxMarkets > 0 && len(result) >= c.maxMarkets {
				break
			}
		}
		if c.maxMarkets > 0 && len(result) >= c.maxMarkets {
			break
		}
	}

	fmt.Printf("[polymarket] Category %q (tag_slug=%s): %d markets from %d events\n",
		category, tagSlug, len(result), len(events))
	return result, nil
}

// FetchMarketsByCategoryWithLimit returns active markets for a category with a per-call limit override.
func (c *Client) FetchMarketsByCategoryWithLimit(ctx context.Context, category string, limit int) ([]*venues.RawMarket, error) {
	tagSlug, ok := polymarketTagSlugs[strings.ToLower(category)]
	if !ok {
		return nil, fmt.Errorf("polymarket: unknown category %q", category)
	}

	if limit <= 0 {
		limit = 50
	}

	u := fmt.Sprintf("%s/events?tag_slug=%s&active=true&closed=false&limit=%d&order=volume24hr&ascending=false",
		c.baseURL, url.QueryEscape(tagSlug), limit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, u)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	var events []struct {
		Markets []json.RawMessage `json:"markets"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("unmarshalling events: %w", err)
	}

	var result []*venues.RawMarket
	seen := map[string]struct{}{}
	for _, ev := range events {
		for _, item := range ev.Markets {
			var m polymarketMarket
			if err := json.Unmarshal(item, &m); err != nil {
				continue
			}
			if !m.Active || m.Closed || m.ID == "" {
				continue
			}
			if _, ok := seen[m.ID]; ok {
				continue
			}
			seen[m.ID] = struct{}{}
			result = append(result, &venues.RawMarket{
				VenueID:       models.VenuePolymarket,
				VenueMarketID: m.ID,
				FetchCategory: category,
				Payload:       item,
			})
			if len(result) >= limit {
				break
			}
		}
		if len(result) >= limit {
			break
		}
	}

	fmt.Printf("[polymarket] Category %q (limit=%d): %d markets from %d events\n",
		category, limit, len(result), len(events))
	return result, nil
}

// fetchMarketsByKeyword fetches active markets and filters client-side by keyword.
// This is slower than public-search but catches markets that public-search misses
// (e.g., when all public-search results are closed events).
func (c *Client) fetchMarketsByKeyword(ctx context.Context, query string) ([]*venues.RawMarket, error) {
	queryLower := strings.ToLower(query)
	queryWords := strings.Fields(queryLower)

	// Only scan up to 500 markets to avoid excessive API calls
	scanLimit := 500
	if c.maxMarkets > 0 && c.maxMarkets < scanLimit {
		scanLimit = c.maxMarkets
	}

	matchLimit := 20 // max keyword-matched results to return
	if c.maxMarkets > 0 && c.maxMarkets < matchLimit {
		matchLimit = c.maxMarkets
	}

	var matched []*venues.RawMarket
	offset := 0

	for len(matched) < matchLimit && offset < scanLimit {
		batchSize := pageSize
		remaining := scanLimit - offset
		if remaining < batchSize {
			batchSize = remaining
		}

		u := fmt.Sprintf("%s/markets?active=true&closed=false&limit=%d&offset=%d&order=volume24hr&ascending=false",
			c.baseURL, batchSize, offset)

		batch, err := c.fetchPage(ctx, u, func(m polymarketMarket) bool {
			// Client-side keyword filter: check if question/description contains any query word
			text := strings.ToLower(m.Question + " " + m.Description + " " + m.Category)
			for _, w := range queryWords {
				if len(w) > 2 && strings.Contains(text, w) {
					return true
				}
			}
			return false
		})
		if err != nil {
			if len(matched) > 0 {
				break // return partial results
			}
			return nil, err
		}
		matched = append(matched, batch...)
		if len(batch) == 0 || len(batch) < batchSize {
			break // last page or empty
		}
		offset += pageSize
	}

	if len(matched) > matchLimit {
		matched = matched[:matchLimit]
	}
	fmt.Printf("[polymarket] Keyword filter for %q: scanned %d, matched %d\n", query, offset+pageSize, len(matched))
	return matched, nil
}

func (c *Client) fetchMarketsWithFilter(ctx context.Context, keep func(polymarketMarket) bool) ([]*venues.RawMarket, error) {
	var all []*venues.RawMarket
	offset := 0

	for {
		if c.maxMarkets > 0 && len(all) >= c.maxMarkets {
			break
		}
		limit := pageSize
		if c.maxMarkets > 0 {
			remaining := c.maxMarkets - len(all)
			if remaining < pageSize {
				limit = remaining
			}
		}

		url := fmt.Sprintf("%s/markets?active=true&closed=false&limit=%d&offset=%d&order=volume24hr&ascending=false",
			c.baseURL, limit, offset)

		batch, err := c.fetchPage(ctx, url, keep)
		if err != nil {
			// If we already have data, log warning and return partial results
			if len(all) > 0 {
				fmt.Printf("[polymarket] WARNING: pagination stopped after %d markets: %v\n", len(all), err)
				break
			}
			return nil, fmt.Errorf("polymarket: FetchMarkets (offset=%d): %w", offset, err)
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		if len(batch) < limit {
			break // last page
		}
		if len(all)%500 == 0 {
			fmt.Printf("[polymarket] Fetched %d markets so far...\n", len(all))
		}
		offset += pageSize
	}

	return all, nil
}

// polymarketMarket is the raw shape returned by Polymarket's Gamma API.
// Only fields we use downstream are defined here; the full payload is retained as RawPayload.
type polymarketMarket struct {
	ID              string  `json:"id"`
	Question        string  `json:"question"`
	Description     string  `json:"description"`
	EndDateISO      string  `json:"endDateIso"`
	Active          bool    `json:"active"`
	Closed          bool    `json:"closed"`
	OutcomePrices   string  `json:"outcomePrices"` // JSON array string e.g. "[\"0.62\", \"0.38\"]"
	Volume          string  `json:"volume"`    // API returns string
	Liquidity       string  `json:"liquidity"` // API returns string
	Category        string  `json:"category"`
	Tags            []struct {
		Label string `json:"label"`
	} `json:"tags"`
}

func (c *Client) fetchPage(ctx context.Context, url string, keep func(polymarketMarket) bool) ([]*venues.RawMarket, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshalling market list: %w", err)
	}

	result := make([]*venues.RawMarket, 0, len(raw))
	for _, item := range raw {
		var m polymarketMarket
		if err := json.Unmarshal(item, &m); err != nil {
			// Log and skip malformed records rather than failing the entire fetch.
			// Assumption: partial data is preferable to a complete failure when
			// one malformed record is returned by the venue.
			fmt.Printf("[polymarket] WARNING: skipping malformed market: %v\n", err)
			continue
		}
		if !m.Active || m.Closed {
			continue
		}
		if keep != nil && !keep(m) {
			continue
		}
		result = append(result, &venues.RawMarket{
			VenueID:       models.VenuePolymarket,
			VenueMarketID: m.ID,
			Payload:       item,
		})
	}

	return result, nil
}

// publicSearchResponse matches the actual Polymarket public-search API response.
type publicSearchResponse struct {
	Events []publicSearchEvent `json:"events"`
}

type publicSearchEvent struct {
	Title   string               `json:"title"`
	Slug    string               `json:"slug"`
	EndDate string               `json:"endDate"`
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

func (c *Client) fetchPublicSearch(ctx context.Context, searchURL string) ([]*venues.RawMarket, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET %s: %w", searchURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, searchURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	var searchResp publicSearchResponse
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return nil, fmt.Errorf("unmarshalling public-search response: %w", err)
	}

	var result []*venues.RawMarket
	seen := map[string]struct{}{}

	for _, ev := range searchResp.Events {
		for _, mkt := range ev.Markets {
			if mkt.Closed || mkt.Slug == "" {
				continue
			}
			if _, ok := seen[mkt.Slug]; ok {
				continue
			}
			seen[mkt.Slug] = struct{}{}

			// Convert outcomePrices array to JSON string for normalizer compatibility
			// e.g. ["0.935","0.065"] → "[\"0.935\",\"0.065\"]"
			outcomePricesStr := ""
			if len(mkt.OutcomePrices) > 0 {
				b, _ := json.Marshal(mkt.OutcomePrices)
				outcomePricesStr = string(b)
			}

			// Build a payload the normalizer's polymarketRaw struct can parse
			payload := map[string]interface{}{
				"id":            mkt.Slug, // public-search has no id, use slug
				"slug":          mkt.Slug,
				"question":      mkt.Question,
				"endDateIso":    ev.EndDate,
				"active":        mkt.Active,
				"closed":        mkt.Closed,
				"outcomePrices": outcomePricesStr,
				"bestBid":       mkt.BestBid,
				"bestAsk":       mkt.BestAsk,
				"spread":        mkt.Spread,
			}

			b, err := json.Marshal(payload)
			if err != nil {
				continue
			}

			result = append(result, &venues.RawMarket{
				VenueID:       models.VenuePolymarket,
				VenueMarketID: mkt.Slug,
				Payload:       b,
			})

			if c.maxMarkets > 0 && len(result) >= c.maxMarkets {
				break
			}
		}
		if c.maxMarkets > 0 && len(result) >= c.maxMarkets {
			break
		}
	}

	fmt.Printf("[polymarket] public-search returned %d markets from %d events\n", len(result), len(searchResp.Events))
	for i, r := range result {
		if i >= 5 {
			fmt.Printf("[polymarket]   ... and %d more\n", len(result)-5)
			break
		}
		var p struct {
			Question string  `json:"question"`
			BestBid  float64 `json:"bestBid"`
			BestAsk  float64 `json:"bestAsk"`
		}
		json.Unmarshal(r.Payload, &p)
		fmt.Printf("[polymarket]   [%d] %s (bid=%.3f ask=%.3f)\n", i+1, p.Question, p.BestBid, p.BestAsk)
	}
	return result, nil
}
