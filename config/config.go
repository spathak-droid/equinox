package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for Equinox.
// Values are loaded from environment variables so no secrets are hardcoded.
type Config struct {
	// OpenAI — used for embedding-based equivalence matching
	OpenAIAPIKey string

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

	// Embedding model — OpenAI model name
	EmbeddingModel string
	// Embedding cache
	EmbeddingCacheEnabled bool
	EmbeddingCachePath    string

	// HTTP timeout for venue API calls
	HTTPTimeout time.Duration
}

// Load reads configuration from environment variables and applies defaults.
func Load() (*Config, error) {
	cfg := &Config{
		// Secrets
		OpenAIAPIKey: os.Getenv("OPENAI_API_KEY"),
		KalshiAPIKey: os.Getenv("KALSHI_API_KEY"),

		// Defaults — overridable via env
		MatchThreshold:         envFloat("MATCH_THRESHOLD", 0.45),
		ProbableMatchThreshold: envFloat("PROBABLE_MATCH_THRESHOLD", 0.35),
		MaxDateDeltaDays:       envInt("MAX_DATE_DELTA_DAYS", 365),

		PriceWeight:     envFloat("PRICE_WEIGHT", 0.60),
		LiquidityWeight: envFloat("LIQUIDITY_WEIGHT", 0.30),
		SpreadWeight:    envFloat("SPREAD_WEIGHT", 0.10),

		DefaultOrderSize: envFloat("DEFAULT_ORDER_SIZE", 1000.0),
		EmbeddingModel:   envString("EMBEDDING_MODEL", "text-embedding-3-small"),
		EmbeddingCacheEnabled: envBool("EMBEDDING_CACHE_ENABLED", false),
		EmbeddingCachePath:    envString("EMBEDDING_CACHE_PATH", ".equinox_embedding_cache.json"),
		HTTPTimeout:      time.Duration(envInt("HTTP_TIMEOUT_SECONDS", 30)) * time.Second,
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
	if c.OpenAIAPIKey == "" {
		// Non-fatal: system will fall back to rule-only matching
		fmt.Println("[config] WARNING: OPENAI_API_KEY not set — embedding matching disabled, using rules only")
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

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		l := strings.ToLower(strings.TrimSpace(v))
		return l == "1" || l == "true" || l == "yes" || l == "on"
	}
	return def
}
