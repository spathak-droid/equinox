// Package kalshi implements the Venue interface for Kalshi's public REST API.
// API docs: https://api.elections.kalshi.com/trade-api/v2
//
// Kalshi requires an API key for most endpoints, but market metadata is available
// without authentication. Pricing data requires auth.
//
// Kalshi markets use "tickers" as identifiers and price in cents (0–100).
// We normalize prices to [0.0, 1.0] in the normalizer, not here.
package kalshi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/equinox/internal/models"
	"github.com/equinox/internal/venues"
)

const (
	baseURL  = "https://api.elections.kalshi.com/trade-api/v2"
	pageSize = 200
)

// Client is a Kalshi REST API client.
type Client struct {
	http       *http.Client
	baseURL    string
	apiKey     string // optional; enables pricing endpoints
	maxMarkets int
	eventTitles map[string]string
	eventInfoCache map[string]*kalshiEvent // event_ticker → event info from API lookups
	eventLookupDisabledUntil time.Time
	eventLookupRateLimited   bool
	eventsCache              []kalshiEvent
	eventsCacheExpiry        time.Time
}

// New returns a new Kalshi client. apiKey may be empty for public-only access.
func New(apiKey string, timeout time.Duration, maxMarkets ...int) *Client {
	limit := 500
	if len(maxMarkets) > 0 && maxMarkets[0] > 0 {
		limit = maxMarkets[0]
	}
	return &Client{
		http:           &http.Client{Timeout: timeout},
		baseURL:        baseURL,
		apiKey:         apiKey,
		maxMarkets:     limit,
		eventTitles:    map[string]string{},
		eventInfoCache: map[string]*kalshiEvent{},
	}
}

// ID implements venues.Venue.
func (c *Client) ID() models.VenueID {
	return models.VenueKalshi
}

// FetchMarkets retrieves active markets from Kalshi.
// It uses a two-pass strategy:
//  1. Load events index and prioritize non-sports events (political, economic, etc.)
//     to maximize overlap with other venues like Polymarket.
//  2. Fall back to generic pagination if event-based fetch yields too few markets.
func (c *Client) FetchMarkets(ctx context.Context) ([]*venues.RawMarket, error) {
	// Pass 1: event-based fetch prioritizing non-sports categories
	events, err := c.loadEventsIndex(ctx)
	if err != nil {
		fmt.Printf("[kalshi] WARNING: event index load failed: %v — falling back to generic fetch\n", err)
		return c.fetchMarketsWithFilter(ctx, nil)
	}

	// Partition events: non-sports first, then sports
	var priorityEvents, sportsEvents []kalshiEvent
	for _, ev := range events {
		if isSportsEvent(ev) {
			sportsEvents = append(sportsEvents, ev)
		} else {
			priorityEvents = append(priorityEvents, ev)
		}
	}
	fmt.Printf("[kalshi] event index: %d non-sports, %d sports events\n", len(priorityEvents), len(sportsEvents))

	// Fetch markets from priority (non-sports) events first
	var all []*venues.RawMarket
	seen := map[string]struct{}{}

	for _, ev := range priorityEvents {
		if len(all) >= c.maxMarkets {
			break
		}
		batch, err := c.fetchMarketsForEvent(ctx, ev.EventTicker)
		if err != nil {
			fmt.Printf("[kalshi] WARNING: skipping event %s: %v\n", ev.EventTicker, err)
			continue
		}
		for _, m := range batch {
			if _, ok := seen[m.VenueMarketID]; ok {
				continue
			}
			seen[m.VenueMarketID] = struct{}{}
			all = append(all, m)
			if len(all) >= c.maxMarkets {
				break
			}
		}
	}

	// If we have room, add some sports markets too
	if len(all) < c.maxMarkets {
		remaining := c.maxMarkets - len(all)
		generic, err := c.fetchMarketsWithFilter(ctx, nil)
		if err == nil {
			added := 0
			for _, m := range generic {
				if added >= remaining {
					break
				}
				if _, ok := seen[m.VenueMarketID]; ok {
					continue
				}
				seen[m.VenueMarketID] = struct{}{}
				all = append(all, m)
				added++
			}
		}
	}

	return all, nil
}

// isSportsEvent returns true if the event appears to be a sports betting market.
func isSportsEvent(ev kalshiEvent) bool {
	upper := strings.ToUpper(ev.SeriesTicker)
	// Known sports series prefixes on Kalshi
	sportsPrefixes := []string{
		"KXNCAAMB",  // NCAA basketball
		"KXNBA",     // NBA
		"KXNFL",     // NFL
		"KXNHL",     // NHL
		"KXMLB",     // MLB
		"KXMLS",     // MLS
		"KXMVESPORT", // Multi-venue sports
		"KXSOCCER",  // Soccer
		"KXUFC",     // UFC
		"KXBOXING",  // Boxing
		"KXTENNIS",  // Tennis
		"KXGOLF",    // Golf
		"KXF1",      // Formula 1
		"KXNASCAR",  // NASCAR
	}
	for _, prefix := range sportsPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	// Also check title keywords
	titleLower := strings.ToLower(ev.Title)
	sportsKeywords := []string{"wins by over", "total points", "spread", "moneyline", "over/under"}
	for _, kw := range sportsKeywords {
		if strings.Contains(titleLower, kw) {
			return true
		}
	}
	return false
}

// fetchMarketsForEvent fetches all active markets for a given event ticker.
func (c *Client) fetchMarketsForEvent(ctx context.Context, eventTicker string) ([]*venues.RawMarket, error) {
	cursor := ""
	var all []*venues.RawMarket
	for {
		limit := pageSize
		u := fmt.Sprintf("%s/markets?status=open&limit=%d&event_ticker=%s", c.baseURL, limit, url.QueryEscape(eventTicker))
		if cursor != "" {
			u += "&cursor=" + cursor
		}
		batch, nextCursor, err := c.fetchPage(ctx, u, nil)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if nextCursor == "" || len(batch) == 0 {
			break
		}
		cursor = nextCursor
	}
	return all, nil
}

// FetchMarketsByQuery resolves text query through Kalshi v1 series search first,
// then fetches markets via v2 using series_ticker filters.
func (c *Client) FetchMarketsByQuery(ctx context.Context, query string) ([]*venues.RawMarket, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return []*venues.RawMarket{}, nil
	}

	seriesTickers, err := c.searchSeriesTickersV1(ctx, q, 25)
	if err != nil {
		return nil, err
	}
	fmt.Printf("[kalshi] series search q=%q returned %d series tickers\n", q, len(seriesTickers))
	if len(seriesTickers) == 0 {
		// Fallback to event-based local search if v1 series search returns nothing.
		eventTickers, fallbackErr := c.findEventTickersByQuery(ctx, q, 8)
		if fallbackErr != nil {
			return nil, fallbackErr
		}
		fmt.Printf("[kalshi] fallback event resolver q=%q returned %d event tickers\n", q, len(eventTickers))
		if len(eventTickers) == 0 {
			return []*venues.RawMarket{}, nil
		}
		return c.fetchMarketsByEventTickers(ctx, eventTickers)
	}
	return c.fetchMarketsBySeriesTickers(ctx, seriesTickers)
}

func (c *Client) fetchMarketsBySeriesTickers(ctx context.Context, seriesTickers []string) ([]*venues.RawMarket, error) {
	var all []*venues.RawMarket
	seen := map[string]struct{}{}
	for _, seriesTicker := range seriesTickers {
		cursor := ""
		for {
			remaining := c.maxMarkets - len(all)
			if remaining <= 0 {
				return all, nil
			}
			limit := pageSize
			if remaining < pageSize {
				limit = remaining
			}
			u := fmt.Sprintf("%s/markets?status=open&limit=%d&series_ticker=%s", c.baseURL, limit, url.QueryEscape(seriesTicker))
			if cursor != "" {
				u += "&cursor=" + cursor
			}
			batch, nextCursor, err := c.fetchPage(ctx, u, nil)
			if err != nil {
				return nil, fmt.Errorf("kalshi: FetchMarketsByQuery series_ticker=%s: %w", seriesTicker, err)
			}
			if len(batch) == 0 {
				break
			}
			for _, m := range batch {
				if _, ok := seen[m.VenueMarketID]; ok {
					continue
				}
				seen[m.VenueMarketID] = struct{}{}
				all = append(all, m)
				if len(all) >= c.maxMarkets {
					return all, nil
				}
			}
			if nextCursor == "" {
				break
			}
			cursor = nextCursor
		}
	}
	return all, nil
}

func (c *Client) fetchMarketsByEventTickers(ctx context.Context, eventTickers []string) ([]*venues.RawMarket, error) {
	var all []*venues.RawMarket
	seen := map[string]struct{}{}
	for _, eventTicker := range eventTickers {
		cursor := ""
		for {
			remaining := c.maxMarkets - len(all)
			if remaining <= 0 {
				return all, nil
			}
			limit := pageSize
			if remaining < pageSize {
				limit = remaining
			}
			u := fmt.Sprintf("%s/markets?status=open&limit=%d&event_ticker=%s", c.baseURL, limit, url.QueryEscape(eventTicker))
			if cursor != "" {
				u += "&cursor=" + cursor
			}
			batch, nextCursor, err := c.fetchPage(ctx, u, nil)
			if err != nil {
				return nil, fmt.Errorf("kalshi: FetchMarketsByQuery event_ticker=%s: %w", eventTicker, err)
			}
			if len(batch) == 0 {
				break
			}
			for _, m := range batch {
				if _, ok := seen[m.VenueMarketID]; ok {
					continue
				}
				seen[m.VenueMarketID] = struct{}{}
				all = append(all, m)
				if len(all) >= c.maxMarkets {
					return all, nil
				}
			}
			if nextCursor == "" {
				break
			}
			cursor = nextCursor
		}
	}
	return all, nil
}

type kalshiFilterFunc func(ctx context.Context, m kalshiMarket) bool

func (c *Client) fetchMarketsWithFilter(ctx context.Context, keep kalshiFilterFunc) ([]*venues.RawMarket, error) {
	var all []*venues.RawMarket
	cursor := ""

	for {
		remaining := c.maxMarkets - len(all)
		if remaining <= 0 {
			break
		}
		limit := pageSize
		if remaining < pageSize {
			limit = remaining
		}
		url := fmt.Sprintf("%s/markets?status=open&limit=%d", c.baseURL, limit)
		if cursor != "" {
			url += "&cursor=" + cursor
		}

		batch, nextCursor, err := c.fetchPage(ctx, url, keep)
		if err != nil {
			return nil, fmt.Errorf("kalshi: FetchMarkets: %w", err)
		}
		all = append(all, batch...)
		if nextCursor == "" || len(all) >= c.maxMarkets {
			break
		}
		cursor = nextCursor
	}

	return all, nil
}

// kalshiMarket is the raw shape from Kalshi's /markets endpoint.
type kalshiMarket struct {
	Ticker        string  `json:"ticker"`
	EventTicker   string  `json:"event_ticker"`
	Title         string  `json:"title"`
	Subtitle      string  `json:"subtitle"`
	Status        string  `json:"status"`
	CloseTime     string  `json:"close_time"`
	YesBid        int     `json:"yes_bid"`  // cents (0–100)
	YesAsk        int     `json:"yes_ask"`  // cents (0–100)
	NoBid         int     `json:"no_bid"`
	NoAsk         int     `json:"no_ask"`
	Volume        float64 `json:"volume"`
	Volume24h     float64 `json:"volume_24h"`
	OpenInterest  float64 `json:"open_interest"`
	Liquidity     float64 `json:"liquidity"`
	RulesPrimary  string  `json:"rules_primary"`
}

type kalshiResponse struct {
	Markets []json.RawMessage `json:"markets"`
	Cursor  string            `json:"cursor"`
}

type kalshiEventResponse struct {
	Title string `json:"title"`
	Event struct {
		Title        string `json:"title"`
		SeriesTicker string `json:"series_ticker"`
		EventTicker  string `json:"event_ticker"`
	} `json:"event"`
}

type kalshiEventsResponse struct {
	Events []json.RawMessage `json:"events"`
	Cursor string            `json:"cursor"`
}

type kalshiEvent struct {
	EventTicker  string `json:"event_ticker"`
	SeriesTicker string `json:"series_ticker"`
	Title        string `json:"title"`
	SubTitle     string `json:"sub_title"`
	Status       string `json:"status"`
}

func (c *Client) newReq(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "equinox-prototype/1.0")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

func (c *Client) searchSeriesTickersV1(ctx context.Context, query string, pageSize int) ([]string, error) {
	if pageSize <= 0 {
		pageSize = 25
	}
	v1URL := fmt.Sprintf(
		"https://api.elections.kalshi.com/v1/search/series?query=%s&order_by=querymatch&page_size=%d&fuzzy_threshold=1&with_milestones=true",
		url.QueryEscape(strings.TrimSpace(query)),
		pageSize,
	)

	req, err := c.newReq(ctx, v1URL)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET %s: %w", v1URL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading v1 series search body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s: %s", resp.StatusCode, v1URL, string(body))
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("unmarshalling v1 series search: %w", err)
	}

	var groups [][]json.RawMessage
	for _, key := range []string{"current_page", "series", "results", "data"} {
		if raw, ok := root[key]; ok {
			var arr []json.RawMessage
			if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
				groups = append(groups, arr)
			}
		}
	}

	// Build query keywords for client-side relevance filtering.
	queryWords := kalshiQueryKeywords(query)

	seen := map[string]struct{}{}
	out := make([]string, 0, pageSize)
	for _, arr := range groups {
		for _, item := range arr {
			var obj map[string]interface{}
			if err := json.Unmarshal(item, &obj); err != nil {
				continue
			}
			ticker := firstNonEmptyKalshi(
				asStringKalshi(obj["series_ticker"]),
				asStringKalshi(obj["ticker"]),
			)
			if ticker == "" {
				continue
			}
			// Client-side relevance check: the series title must contain
			// at least one query keyword. This filters out fuzzy false
			// positives from the Kalshi V1 search API (e.g. "Neal" for "Nepal").
			title := strings.ToLower(firstNonEmptyKalshi(
				asStringKalshi(obj["title"]),
				asStringKalshi(obj["name"]),
				asStringKalshi(obj["series_title"]),
				ticker,
			))
			if !kalshiTitleMatchesQuery(title, ticker, queryWords) {
				continue
			}
			ticker = strings.ToUpper(strings.TrimSpace(ticker))
			if _, ok := seen[ticker]; ok {
				continue
			}
			seen[ticker] = struct{}{}
			out = append(out, ticker)
		}
	}
	return out, nil
}

func (c *Client) fetchPage(ctx context.Context, url string, keep kalshiFilterFunc) ([]*venues.RawMarket, string, error) {
	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(time.Duration(attempt*2) * time.Second):
			}
		}
		req, err := c.newReq(ctx, url)
		if err != nil {
			return nil, "", err
		}
		var doErr error
		resp, doErr = c.http.Do(req)
		if doErr != nil {
			return nil, "", fmt.Errorf("HTTP GET %s: %w", url, doErr)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			break
		}
		resp.Body.Close()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading response body: %w", err)
	}

	var page kalshiResponse
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, "", fmt.Errorf("unmarshalling kalshi response: %w", err)
	}

	result := make([]*venues.RawMarket, 0, len(page.Markets))
	for _, item := range page.Markets {
		var m kalshiMarket
		if err := json.Unmarshal(item, &m); err != nil {
			fmt.Printf("[kalshi] WARNING: skipping malformed market: %v\n", err)
			continue
		}
		if m.Status != "active" {
			continue
		}
		if isSyntheticCrossCategoryMarket(m) {
			continue
		}
		if keep != nil && !keep(ctx, m) {
			continue
		}

		payload := item
		{
			var obj map[string]interface{}
			needsMarshal := false
			if err := json.Unmarshal(item, &obj); err == nil {
				// Always try to resolve event info (title is cached after first call per event)
				var eventTitle string
				if m.EventTicker != "" {
					t, err := c.getEventTitle(ctx, m.EventTicker)
					if err != nil {
						fmt.Printf("[kalshi] WARNING: event title lookup failed for %s: %v\n", m.EventTicker, err)
					} else {
						eventTitle = strings.TrimSpace(t)
					}
				}

				// Inject event title as market title when market title is unhelpful
				if shouldUseEventTitle(m.Title, m.Subtitle, m.EventTicker) && eventTitle != "" {
					obj["title"] = eventTitle
					needsMarshal = true
				}

				// Inject series_ticker and event_title for frontend URL construction
				if ev := c.lookupEvent(m.EventTicker); ev != nil {
					if ev.SeriesTicker != "" {
						obj["series_ticker"] = ev.SeriesTicker
						needsMarshal = true
					}
					if ev.Title != "" {
						obj["event_title"] = ev.Title
						needsMarshal = true
					}
				} else if m.EventTicker != "" {
					// Derive series_ticker from event_ticker (strip last -SUFFIX)
					if idx := strings.LastIndex(m.EventTicker, "-"); idx > 0 {
						obj["series_ticker"] = m.EventTicker[:idx]
						needsMarshal = true
					}
					if eventTitle != "" {
						obj["event_title"] = eventTitle
						needsMarshal = true
					}
				}
				if needsMarshal {
					if b, err := json.Marshal(obj); err == nil {
						payload = b
					}
				}
			}
		}

		result = append(result, &venues.RawMarket{
			VenueID:       models.VenueKalshi,
			VenueMarketID: m.Ticker,
			Payload:       payload,
		})
	}

	return result, page.Cursor, nil
}

// lookupEvent returns a cached kalshiEvent for the given event ticker, or nil.
// Checks both the bulk events cache and the per-event info cache (populated by getEventTitle).
func (c *Client) lookupEvent(eventTicker string) *kalshiEvent {
	for i := range c.eventsCache {
		if c.eventsCache[i].EventTicker == eventTicker {
			return &c.eventsCache[i]
		}
	}
	if ev, ok := c.eventInfoCache[eventTicker]; ok {
		return ev
	}
	return nil
}

func shouldUseEventTitle(title, subtitle, eventTicker string) bool {
	title = strings.TrimSpace(strings.ToLower(title))
	subtitle = strings.TrimSpace(subtitle)
	if title == "" || eventTicker == "" {
		return false
	}
	if subtitle != "" {
		return false
	}
	if !strings.Contains(title, ",") {
		return false
	}
	return strings.HasPrefix(title, "yes ") || strings.HasPrefix(title, "no ")
}

func (c *Client) getEventTitle(ctx context.Context, eventTicker string) (string, error) {
	if t, ok := c.eventTitles[eventTicker]; ok {
		return t, nil
	}
	if time.Now().Before(c.eventLookupDisabledUntil) {
		return "", nil
	}

	url := fmt.Sprintf("%s/events/%s", c.baseURL, eventTicker)
	req, err := c.newReq(ctx, url)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		c.eventLookupDisabledUntil = time.Now().Add(2 * time.Minute)
		if !c.eventLookupRateLimited {
			fmt.Printf("[kalshi] WARNING: event title lookups paused for 2m due to API 429 rate limit\n")
			c.eventLookupRateLimited = true
		}
		return "", nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}
	c.eventLookupRateLimited = false

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}

	var ev kalshiEventResponse
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", fmt.Errorf("unmarshalling event response: %w", err)
	}
	title := strings.TrimSpace(ev.Event.Title)
	if title == "" {
		title = strings.TrimSpace(ev.Title)
	}
	c.eventTitles[eventTicker] = title

	// Cache full event info for URL construction
	if ev.Event.SeriesTicker != "" || ev.Event.EventTicker != "" {
		c.eventInfoCache[eventTicker] = &kalshiEvent{
			EventTicker:  ev.Event.EventTicker,
			SeriesTicker: ev.Event.SeriesTicker,
			Title:        title,
		}
	}

	return title, nil
}

func (c *Client) findEventTickersByQuery(ctx context.Context, query string, maxTickers int) ([]string, error) {
	events, err := c.loadEventsIndex(ctx)
	if err != nil {
		return nil, fmt.Errorf("kalshi: loading events index: %w", err)
	}

	q := normalizeForMatch(query)
	if q == "" {
		return nil, nil
	}
	qTokens := strings.Fields(q)

	type scoredEvent struct {
		event kalshiEvent
		score int
	}
	scored := make([]scoredEvent, 0, len(events))
	for _, ev := range events {
		title := normalizeForMatch(ev.Title)
		subTitle := normalizeForMatch(ev.SubTitle)
		series := normalizeForMatch(ev.SeriesTicker)
		ticker := normalizeForMatch(ev.EventTicker)
		score := 0
		if strings.Contains(title, q) {
			score += 8
		}
		if strings.Contains(subTitle, q) {
			score += 5
		}
		if strings.Contains(ticker, q) {
			score += 6
		}
		if strings.Contains(series, q) {
			score += 3
		}
		for _, tok := range qTokens {
			if tok == "" {
				continue
			}
			if strings.Contains(title, tok) {
				score++
			}
			if strings.Contains(subTitle, tok) {
				score++
			}
		}
		if score > 0 {
			scored = append(scored, scoredEvent{event: ev, score: score})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score == scored[j].score {
			return scored[i].event.EventTicker < scored[j].event.EventTicker
		}
		return scored[i].score > scored[j].score
	})

	if len(scored) > maxTickers {
		scored = scored[:maxTickers]
	}

	out := make([]string, 0, len(scored))
	for _, se := range scored {
		out = append(out, se.event.EventTicker)
	}
	return out, nil
}

func (c *Client) loadEventsIndex(ctx context.Context) ([]kalshiEvent, error) {
	if time.Now().Before(c.eventsCacheExpiry) && len(c.eventsCache) > 0 {
		return c.eventsCache, nil
	}

	const maxEventScan = 1200
	events := make([]kalshiEvent, 0, 400)
	cursor := ""
	for len(events) < maxEventScan {
		limit := 200
		u := fmt.Sprintf("%s/events?limit=%d", c.baseURL, limit)
		if cursor != "" {
			u += "&cursor=" + cursor
		}

		req, err := c.newReq(ctx, u)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HTTP GET %s: %w", u, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("reading events response body: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			// Graceful degradation: if we've already ingested at least one page,
			// keep partial index instead of failing the whole search flow.
			if len(events) > 0 {
				fmt.Printf("[kalshi] WARNING: events index partial due to status %d from %s; using %d cached candidates\n",
					resp.StatusCode, u, len(events))
				break
			}
			return nil, fmt.Errorf("unexpected status %d from %s: %s", resp.StatusCode, u, string(body))
		}

		var page kalshiEventsResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("unmarshalling events response: %w", err)
		}
		if len(page.Events) == 0 {
			break
		}

		for _, raw := range page.Events {
			var ev kalshiEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				continue
			}
			if strings.TrimSpace(ev.EventTicker) == "" {
				continue
			}
			events = append(events, ev)
			if len(events) >= maxEventScan {
				break
			}
		}

		if page.Cursor == "" || len(page.Events) < limit {
			break
		}
		cursor = page.Cursor
	}

	c.eventsCache = events
	c.eventsCacheExpiry = time.Now().Add(5 * time.Minute)
	return events, nil
}

func normalizeForMatch(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	return strings.Join(strings.Fields(s), " ")
}

func isSyntheticCrossCategoryMarket(m kalshiMarket) bool {
	upper := strings.ToUpper(strings.TrimSpace(m.EventTicker))
	// Filter out cross-category and multi-game combo markets
	if strings.HasPrefix(upper, "KXMVECROSSCATEGORY-") {
		return true
	}
	if strings.HasPrefix(upper, "KXMVESPORTSMULTIGAME") {
		return true
	}
	if strings.HasPrefix(upper, "KXMVESPORTS") && strings.Contains(upper, "MULTI") {
		return true
	}

	t := strings.ToLower(strings.TrimSpace(m.Title))
	// Filter out markets with useless titles
	if t == "combo" || t == "" {
		return true
	}
	if (strings.HasPrefix(t, "yes ") || strings.HasPrefix(t, "no ")) && strings.Count(t, ",") >= 3 {
		return true
	}
	return false
}

func asStringKalshi(v interface{}) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	default:
		return ""
	}
}

func firstNonEmptyKalshi(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// kalshiQueryKeywords splits a search query into lowercase keywords,
// filtering out very short words (<=2 chars) that cause false positives.
func kalshiQueryKeywords(query string) []string {
	words := strings.Fields(strings.ToLower(query))
	out := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.TrimFunc(w, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
		})
		if len(w) > 2 {
			out = append(out, w)
		}
	}
	return out
}

// kalshiTitleMatchesQuery returns true if the series title or ticker
// contains at least one of the query keywords.
func kalshiTitleMatchesQuery(title, ticker string, queryWords []string) bool {
	if len(queryWords) == 0 {
		return true // no filtering if query is empty/too short
	}
	combined := strings.ToLower(title + " " + ticker)
	for _, kw := range queryWords {
		if strings.Contains(combined, kw) {
			return true
		}
	}
	return false
}
