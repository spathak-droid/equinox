package kalshisearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL      = "https://api.elections.kalshi.com/trade-api/v2"
	defaultLimit        = 20
	maxLimit            = 100
	defaultCacheTTL     = 2 * time.Minute
	maxPagesPerResource = 5
)

// Service wraps Kalshi discovery endpoints and provides a search-like API.
type Service struct {
	http    *http.Client
	baseURL string
	ttl     time.Duration

	mu    sync.RWMutex
	cache indexCache
}

func New(timeout time.Duration) *Service {
	return &Service{
		http:    &http.Client{Timeout: timeout},
		baseURL: defaultBaseURL,
		ttl:     defaultCacheTTL,
	}
}

func (s *Service) Search(ctx context.Context, opts SearchOptions) (*SearchResponse, error) {
	if opts.Limit <= 0 {
		opts.Limit = defaultLimit
	}
	if opts.Limit > maxLimit {
		opts.Limit = maxLimit
	}
	items, cachedAt, refreshed, warnings, err := s.getIndex(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Printf("[kalshi-search] index items=%d refreshed=%v warnings=%d\n", len(items), refreshed, len(warnings))

	query := strings.TrimSpace(opts.Query)
	results := make([]ResultItem, 0, opts.Limit)
	filteredIn := 0
	for _, item := range items {
		if !passesFilters(item, opts) {
			continue
		}
		filteredIn++
		score, matched := ScoreCandidate(item, query)
		if query != "" && !matched {
			continue
		}
		item.Score = score
		results = append(results, item)
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			if results[i].Liquidity == results[j].Liquidity {
				if results[i].Volume == results[j].Volume {
					return results[i].ID < results[j].ID
				}
				return results[i].Volume > results[j].Volume
			}
			return results[i].Liquidity > results[j].Liquidity
		}
		return results[i].Score > results[j].Score
	})

	if len(results) > opts.Limit {
		results = results[:opts.Limit]
	}
	fmt.Printf("[kalshi-search] scoring query=%q filter_pass=%d matched=%d returned=%d\n",
		query, filteredIn, len(results), len(results))

	resp := &SearchResponse{
		Query:     query,
		Count:     len(results),
		Results:   results,
		Warnings:  warnings,
		Refreshed: refreshed,
	}
	if !cachedAt.IsZero() {
		resp.CachedAt = cachedAt.Format(time.RFC3339)
	}
	return resp, nil
}

func passesFilters(item ResultItem, opts SearchOptions) bool {
	if t := strings.ToLower(strings.TrimSpace(opts.Type)); t != "" && t != strings.ToLower(item.Type) {
		return false
	}
	if status := strings.ToLower(strings.TrimSpace(opts.Status)); status != "" {
		itemStatus := strings.ToLower(strings.TrimSpace(item.Status))
		if status == "open" && (itemStatus == "open" || itemStatus == "active") {
			// accepted
		} else if itemStatus != status {
			return false
		}
	}
	if series := strings.ToUpper(strings.TrimSpace(opts.Series)); series != "" &&
		!strings.Contains(strings.ToUpper(item.SeriesTicker), series) {
		return false
	}
	if event := strings.ToUpper(strings.TrimSpace(opts.Event)); event != "" &&
		!strings.Contains(strings.ToUpper(item.EventTicker), event) {
		return false
	}
	return true
}

func (s *Service) getIndex(ctx context.Context) ([]ResultItem, time.Time, bool, []string, error) {
	now := time.Now()
	s.mu.RLock()
	cache := s.cache
	s.mu.RUnlock()
	if len(cache.Items) > 0 && now.Before(cache.ExpiresAt) {
		fmt.Printf("[kalshi-search] cache hit items=%d age=%s\n", len(cache.Items), now.Sub(cache.CachedAt).Round(time.Second))
		return cache.Items, cache.CachedAt, false, nil, nil
	}
	fmt.Printf("[kalshi-search] cache miss, refreshing index\n")

	items, warnings, err := s.refreshIndex(ctx)
	if err != nil {
		// graceful fallback to stale cache
		if len(cache.Items) > 0 {
			warnings = append(warnings, "using stale cache due to refresh failure: "+err.Error())
			fmt.Printf("[kalshi-search] refresh failed, using stale cache items=%d err=%v\n", len(cache.Items), err)
			return cache.Items, cache.CachedAt, false, warnings, nil
		}
		return nil, time.Time{}, false, warnings, fmt.Errorf("kalshi search index unavailable: %w", err)
	}

	newCache := indexCache{
		Items:     items,
		CachedAt:  time.Now(),
		ExpiresAt: time.Now().Add(s.ttl),
	}
	s.mu.Lock()
	s.cache = newCache
	s.mu.Unlock()
	return newCache.Items, newCache.CachedAt, true, warnings, nil
}

func (s *Service) refreshIndex(ctx context.Context) ([]ResultItem, []string, error) {
	var warnings []string
	var all []ResultItem

	marketRaw, err := s.fetchPaginated(ctx, "/markets", "markets", maxPagesPerResource, map[string]string{
		"status": "open",
		"limit":  "200",
	})
	if err != nil {
		warnings = append(warnings, "markets fetch failed: "+err.Error())
	} else {
		fmt.Printf("[kalshi-search] fetched markets raw=%d\n", len(marketRaw))
		all = append(all, normalizeMarkets(marketRaw)...)
	}

	eventRaw, err := s.fetchPaginated(ctx, "/events", "events", maxPagesPerResource, map[string]string{
		"limit": "200",
	})
	if err != nil {
		warnings = append(warnings, "events fetch failed: "+err.Error())
	} else {
		fmt.Printf("[kalshi-search] fetched events raw=%d\n", len(eventRaw))
		all = append(all, normalizeEvents(eventRaw)...)
	}

	seriesRaw, err := s.fetchPaginated(ctx, "/series", "series", maxPagesPerResource, map[string]string{
		"limit": "200",
	})
	if err != nil {
		warnings = append(warnings, "series fetch failed: "+err.Error())
	} else {
		fmt.Printf("[kalshi-search] fetched series raw=%d\n", len(seriesRaw))
		all = append(all, normalizeSeries(seriesRaw)...)
	}

	if len(all) == 0 {
		return nil, warnings, fmt.Errorf("no data fetched from markets/events/series")
	}
	deduped := dedupeByID(all)
	fmt.Printf("[kalshi-search] normalized total=%d deduped=%d\n", len(all), len(deduped))
	return deduped, warnings, nil
}

func (s *Service) fetchPaginated(ctx context.Context, path, listKey string, maxPages int, params map[string]string) ([]json.RawMessage, error) {
	cursor := ""
	out := make([]json.RawMessage, 0, 1024)

	for page := 0; page < maxPages; page++ {
		u, err := url.Parse(s.baseURL + path)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "equinox-kalshi-search/1.0")

		resp, err := s.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", u.String(), err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("reading %s body: %w", u.String(), readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d from %s: %s", resp.StatusCode, u.String(), string(body))
		}

		var pageObj map[string]json.RawMessage
		if err := json.Unmarshal(body, &pageObj); err != nil {
			return nil, fmt.Errorf("decode %s response: %w", path, err)
		}

		var batch []json.RawMessage
		if raw, ok := pageObj[listKey]; ok {
			_ = json.Unmarshal(raw, &batch)
		}
		if len(batch) == 0 {
			break
		}
		out = append(out, batch...)

		var nextCursor string
		if raw, ok := pageObj["cursor"]; ok {
			_ = json.Unmarshal(raw, &nextCursor)
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return out, nil
}

func normalizeMarkets(raw []json.RawMessage) []ResultItem {
	out := make([]ResultItem, 0, len(raw))
	for _, r := range raw {
		m := parseObject(r)
		ticker := getString(m, "ticker")
		if ticker == "" {
			continue
		}
		out = append(out, ResultItem{
			ID:           "market:" + ticker,
			Type:         "market",
			Ticker:       ticker,
			Title:        getString(m, "title"),
			Subtitle:     firstNonEmpty(getString(m, "subtitle"), getString(m, "sub_title")),
			Status:       normalizeStatus(getString(m, "status")),
			EventTicker:  getString(m, "event_ticker"),
			SeriesTicker: firstNonEmpty(getString(m, "series_ticker"), getString(m, "series")),
			Volume:       getFloat(m, "volume"),
			Liquidity:    getFloat(m, "liquidity"),
		})
	}
	return out
}

func normalizeEvents(raw []json.RawMessage) []ResultItem {
	out := make([]ResultItem, 0, len(raw))
	for _, r := range raw {
		e := parseObject(r)
		ticker := firstNonEmpty(getString(e, "event_ticker"), getString(e, "ticker"))
		if ticker == "" {
			continue
		}
		out = append(out, ResultItem{
			ID:           "event:" + ticker,
			Type:         "event",
			Ticker:       ticker,
			Title:        getString(e, "title"),
			Subtitle:     firstNonEmpty(getString(e, "sub_title"), getString(e, "subtitle")),
			Status:       normalizeStatus(getString(e, "status")),
			EventTicker:  ticker,
			SeriesTicker: firstNonEmpty(getString(e, "series_ticker"), getString(e, "series")),
		})
	}
	return out
}

func normalizeSeries(raw []json.RawMessage) []ResultItem {
	out := make([]ResultItem, 0, len(raw))
	for _, r := range raw {
		s := parseObject(r)
		ticker := firstNonEmpty(getString(s, "series_ticker"), getString(s, "ticker"))
		if ticker == "" {
			continue
		}
		out = append(out, ResultItem{
			ID:           "series:" + ticker,
			Type:         "series",
			Ticker:       ticker,
			Title:        getString(s, "title"),
			Subtitle:     firstNonEmpty(getString(s, "subtitle"), getString(s, "sub_title")),
			Status:       normalizeStatus(getString(s, "status")),
			SeriesTicker: ticker,
		})
	}
	return out
}

func dedupeByID(items []ResultItem) []ResultItem {
	seen := make(map[string]struct{}, len(items))
	out := make([]ResultItem, 0, len(items))
	for _, it := range items {
		if _, ok := seen[it.ID]; ok {
			continue
		}
		seen[it.ID] = struct{}{}
		out = append(out, it)
	}
	return out
}

func parseObject(raw json.RawMessage) map[string]interface{} {
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]interface{}{}
	}
	return m
}

func getString(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

func getFloat(m map[string]interface{}, key string) float64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	default:
		return 0
	}
}

func normalizeStatus(s string) string {
	st := strings.ToLower(strings.TrimSpace(s))
	if st == "active" {
		return "open"
	}
	return st
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
