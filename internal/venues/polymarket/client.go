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
	http       *http.Client
	baseURL    string
	maxMarkets int
}

// New returns a new Polymarket client.
func New(timeout time.Duration, maxMarkets ...int) *Client {
	limit := 500
	if len(maxMarkets) > 0 && maxMarkets[0] > 0 {
		limit = maxMarkets[0]
	}
	return &Client{
		http:       &http.Client{Timeout: timeout},
		baseURL:    baseURL,
		maxMarkets: limit,
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
func (c *Client) FetchMarketsByQuery(ctx context.Context, query string) ([]*venues.RawMarket, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return []*venues.RawMarket{}, nil
	}
	u := fmt.Sprintf("%s/public-search?cache=true&q=%s", c.baseURL, url.QueryEscape(q))
	return c.fetchPublicSearch(ctx, u)
}

func (c *Client) fetchMarketsWithFilter(ctx context.Context, keep func(polymarketMarket) bool) ([]*venues.RawMarket, error) {
	var all []*venues.RawMarket
	offset := 0

	for {
		remaining := c.maxMarkets - len(all)
		if remaining <= 0 {
			break
		}
		limit := pageSize
		if remaining < pageSize {
			limit = remaining
		}

		url := fmt.Sprintf("%s/markets?active=true&closed=false&limit=%d&offset=%d",
			c.baseURL, limit, offset)

		batch, err := c.fetchPage(ctx, url, keep)
		if err != nil {
			return nil, fmt.Errorf("polymarket: FetchMarkets (offset=%d): %w", offset, err)
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		if len(batch) < limit || len(all) >= c.maxMarkets {
			break // last page or cap reached
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

	var candidates []json.RawMessage

	var arr []json.RawMessage
	if err := json.Unmarshal(body, &arr); err == nil {
		candidates = append(candidates, arr...)
	} else {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(body, &obj); err != nil {
			return nil, fmt.Errorf("unmarshalling public-search response: %w", err)
		}
		if raw, ok := obj["markets"]; ok {
			var markets []json.RawMessage
			if err := json.Unmarshal(raw, &markets); err == nil {
				candidates = append(candidates, markets...)
			}
		}
		if raw, ok := obj["data"]; ok {
			var data []json.RawMessage
			if err := json.Unmarshal(raw, &data); err == nil {
				candidates = append(candidates, data...)
			}
		}
		if raw, ok := obj["events"]; ok {
			var events []struct {
				Markets []json.RawMessage `json:"markets"`
			}
			if err := json.Unmarshal(raw, &events); err == nil {
				for _, ev := range events {
					candidates = append(candidates, ev.Markets...)
				}
			}
		}
	}

	result := make([]*venues.RawMarket, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, item := range candidates {
		if len(result) >= c.maxMarkets {
			break
		}
		var m polymarketMarket
		if err := json.Unmarshal(item, &m); err != nil {
			continue
		}
		if m.ID == "" || m.Closed {
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
	}
	return result, nil
}
