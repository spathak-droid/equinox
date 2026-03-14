package kalshi

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
	Ticker           string `json:"ticker"`
	YesBid           int    `json:"yes_bid"`
	YesAsk           int    `json:"yes_ask"`
	LastPrice        int    `json:"last_price"`
	Volume           int    `json:"volume"`
	CloseTS          string `json:"close_ts"`
	YesSubtitle      string `json:"yes_subtitle"`
	NoSubtitle       string `json:"no_subtitle"`
	Result           string `json:"result"` // "yes", "no", or "" for active
	Score            int    `json:"score"`
	PriceDelta       int    `json:"price_delta"`
	IconURLLightMode string `json:"icon_url_light_mode"`
	IconURLDarkMode  string `json:"icon_url_dark_mode"`
}

// ─── v2 bulk response types ─────────────────────────────────────────────────

// v2MarketsResponse is the response shape from the v2 /markets endpoint.
type v2MarketsResponse struct {
	Markets []v2Market `json:"markets"`
	Cursor  string     `json:"cursor"`
}

type v2Market struct {
	Ticker           string `json:"ticker"`
	EventTicker      string `json:"event_ticker"`
	Title            string `json:"title"`
	Subtitle         string `json:"subtitle"`
	YesSubTitle      string `json:"yes_sub_title"`
	NoSubTitle       string `json:"no_sub_title"`
	Status           string `json:"status"`
	CloseTime        string `json:"close_time"`
	YesBidDollars    string `json:"yes_bid_dollars"`
	YesAskDollars    string `json:"yes_ask_dollars"`
	NoBidDollars     string `json:"no_bid_dollars"`
	NoAskDollars     string `json:"no_ask_dollars"`
	LastPriceDollars string `json:"last_price_dollars"`
	VolumeFP         string `json:"volume_fp"`
	Volume24hFP      string `json:"volume_24h_fp"`
	OpenInterestFP   string `json:"open_interest_fp"`
	LiquidityDollars string `json:"liquidity_dollars"`
	Result           string `json:"result"`
	Category         string `json:"category"`
	RulesPrimary     string `json:"rules_primary"`
	RulesSecondary   string `json:"rules_secondary"`
}

// ─── Category mappings ──────────────────────────────────────────────────────

// allCategories are the Kalshi categories we sweep to build a broad market set.
var allCategories = []string{
	"Politics", "Crypto", "Economics", "Sports",
	"Science and Technology", "Entertainment", "World", "Financials",
}

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
