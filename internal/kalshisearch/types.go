package kalshisearch

import "time"

// ResultItem is the normalized, ranked search document returned by the wrapper API.
type ResultItem struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"` // market | event | series
	Ticker       string  `json:"ticker"`
	Title        string  `json:"title"`
	Subtitle     string  `json:"subtitle,omitempty"`
	Status       string  `json:"status,omitempty"`
	EventTicker  string  `json:"event_ticker,omitempty"`
	SeriesTicker string  `json:"series_ticker,omitempty"`
	Volume       float64 `json:"volume,omitempty"`
	Liquidity    float64 `json:"liquidity,omitempty"`
	Score        float64 `json:"score"`
}

// SearchResponse is the response envelope returned by the wrapper API.
type SearchResponse struct {
	Query     string       `json:"query"`
	Count     int          `json:"count"`
	Results   []ResultItem `json:"results"`
	Warnings  []string     `json:"warnings,omitempty"`
	CachedAt  string       `json:"cached_at,omitempty"`
	Refreshed bool         `json:"refreshed"`
}

// SearchOptions controls filtering and limits.
type SearchOptions struct {
	Query  string
	Status string
	Limit  int
	Type   string
	Series string
	Event  string
}

type indexCache struct {
	Items     []ResultItem
	ExpiresAt time.Time
	CachedAt  time.Time
}

