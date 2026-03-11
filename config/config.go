package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration for Equinox.
// Values are loaded from environment variables so no secrets are hardcoded.
type Config struct {
	// Venue API keys (Kalshi requires auth; Polymarket public API does not)
	KalshiAPIKey string

	// Matching thresholds
	// MatchThreshold: composite score above which two markets are considered equivalent
	MatchThreshold float64
	// ProbableMatchThreshold: composite score for "likely but uncertain" matches (review/disambiguation stage)
	ProbableMatchThreshold float64
	// MaxDateDeltaDays: markets with resolution dates further apart than this are never matched
	MaxDateDeltaDays int

	// Routing weights (must sum to 1.0)
	// PriceWeight: proportion of routing score derived from best available price
	PriceWeight float64
	// LiquidityWeight: proportion derived from available liquidity
	LiquidityWeight float64
	// SpreadWeight: proportion derived from bid-ask spread (lower spread = better)
	SpreadWeight float64

	// Simulation
	// DefaultOrderSize: hypothetical order size in USD used by the routing simulator
	DefaultOrderSize float64

	// HTTP timeout for venue API calls
	HTTPTimeout time.Duration

	// Per-venue market limits (0 = unlimited, fetch everything)
	PolymarketMaxMarkets int
	KalshiMaxMarkets     int

	// Venue search API URLs
	PolymarketSearchAPI string
	KalshiSearchAPI     string

	// News integration
	NewsEnabled     bool
	NewsMaxArticles int

	// Fetch strategy: "category", "search", or "broad"
	FetchStrategy string
	// Markets per category when using category-bucketed fetch (default 50)
	MarketsPerCategory int
	// Max concurrent HTTP calls during category fetch (default 4)
	FetchConcurrency int
	// Rate limit delay in ms between category fetch calls (default 500)
	FetchRateLimitMs int
}

// Load reads configuration from environment variables and applies defaults.
func Load() (*Config, error) {
	cfg := &Config{
		KalshiAPIKey: os.Getenv("KALSHI_API_KEY"),

		// Defaults — overridable via env
		MatchThreshold:         envFloat("MATCH_THRESHOLD", 0.45),
		ProbableMatchThreshold: envFloat("PROBABLE_MATCH_THRESHOLD", 0.35),
		MaxDateDeltaDays:       envInt("MAX_DATE_DELTA_DAYS", 365),

		PriceWeight:     envFloat("PRICE_WEIGHT", 0.60),
		LiquidityWeight: envFloat("LIQUIDITY_WEIGHT", 0.30),
		SpreadWeight:    envFloat("SPREAD_WEIGHT", 0.10),

		DefaultOrderSize: envFloat("DEFAULT_ORDER_SIZE", 1000.0),
		HTTPTimeout:          time.Duration(envInt("HTTP_TIMEOUT_SECONDS", 30)) * time.Second,
		PolymarketMaxMarkets: envInt("POLYMARKET_MAX_MARKETS", 0),
		KalshiMaxMarkets:     envInt("KALSHI_MAX_MARKETS", 0),

		PolymarketSearchAPI: envString("POLYMARKET_SEARCH_API", "https://gamma-api.polymarket.com/public-search"),
		KalshiSearchAPI:     envString("KALSHI_SEARCH_API", "https://api.elections.kalshi.com/v1/search/series"),

		NewsEnabled:     envBool("NEWS_ENABLED", false),
		NewsMaxArticles: envInt("NEWS_MAX_ARTICLES", 5),

		FetchStrategy:      envString("FETCH_STRATEGY", "category"),
		MarketsPerCategory: envInt("MARKETS_PER_CATEGORY", 50),
		FetchConcurrency:   envInt("FETCH_CONCURRENCY", 4),
		FetchRateLimitMs:   envInt("FETCH_RATE_LIMIT_MS", 500),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	total := c.PriceWeight + c.LiquidityWeight + c.SpreadWeight
	if total < 0.99 || total > 1.01 {
		return fmt.Errorf("routing weights must sum to 1.0, got %.2f", total)
	}
	if c.MatchThreshold <= c.ProbableMatchThreshold {
		return fmt.Errorf("MATCH_THRESHOLD (%.2f) must be > PROBABLE_MATCH_THRESHOLD (%.2f)",
			c.MatchThreshold, c.ProbableMatchThreshold)
	}
	return nil
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "true" || v == "1" || v == "yes"
	}
	return def
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

