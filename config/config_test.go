package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// Clear any env vars that could interfere
	for _, key := range []string{
		"KALSHI_API_KEY", "MATCH_THRESHOLD", "PROBABLE_MATCH_THRESHOLD",
		"MAX_DATE_DELTA_DAYS", "PRICE_WEIGHT", "LIQUIDITY_WEIGHT", "SPREAD_WEIGHT",
		"DEFAULT_ORDER_SIZE", "HTTP_TIMEOUT_SECONDS", "POLYMARKET_MAX_MARKETS",
		"KALSHI_MAX_MARKETS", "FETCH_STRATEGY", "MARKETS_PER_CATEGORY",
		"FETCH_CONCURRENCY", "FETCH_RATE_LIMIT_MS", "OPENAI_API_KEY",
		"OPEN_AI_MODEL", "NEWS_ENABLED", "NEWS_MAX_ARTICLES",
		"EMBEDDING_MODEL", "QDRANT_URL", "QDRANT_API_KEY", "QDRANT_COLLECTION",
		"POLYMARKET_SEARCH_API", "KALSHI_SEARCH_API",
	} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with defaults returned error: %v", err)
	}

	if cfg.MatchThreshold != 0.45 {
		t.Errorf("MatchThreshold = %f, want 0.45", cfg.MatchThreshold)
	}
	if cfg.ProbableMatchThreshold != 0.35 {
		t.Errorf("ProbableMatchThreshold = %f, want 0.35", cfg.ProbableMatchThreshold)
	}
	if cfg.MaxDateDeltaDays != 365 {
		t.Errorf("MaxDateDeltaDays = %d, want 365", cfg.MaxDateDeltaDays)
	}
	if cfg.PriceWeight != 0.60 {
		t.Errorf("PriceWeight = %f, want 0.60", cfg.PriceWeight)
	}
	if cfg.LiquidityWeight != 0.30 {
		t.Errorf("LiquidityWeight = %f, want 0.30", cfg.LiquidityWeight)
	}
	if cfg.SpreadWeight != 0.10 {
		t.Errorf("SpreadWeight = %f, want 0.10", cfg.SpreadWeight)
	}
	if cfg.DefaultOrderSize != 1000.0 {
		t.Errorf("DefaultOrderSize = %f, want 1000.0", cfg.DefaultOrderSize)
	}
	if cfg.HTTPTimeout != 30*time.Second {
		t.Errorf("HTTPTimeout = %v, want 30s", cfg.HTTPTimeout)
	}
	if cfg.PolymarketMaxMarkets != 0 {
		t.Errorf("PolymarketMaxMarkets = %d, want 0", cfg.PolymarketMaxMarkets)
	}
	if cfg.KalshiMaxMarkets != 0 {
		t.Errorf("KalshiMaxMarkets = %d, want 0", cfg.KalshiMaxMarkets)
	}
	if cfg.FetchStrategy != "category" {
		t.Errorf("FetchStrategy = %q, want %q", cfg.FetchStrategy, "category")
	}
	if cfg.MarketsPerCategory != 50 {
		t.Errorf("MarketsPerCategory = %d, want 50", cfg.MarketsPerCategory)
	}
	if cfg.FetchConcurrency != 4 {
		t.Errorf("FetchConcurrency = %d, want 4", cfg.FetchConcurrency)
	}
	if cfg.FetchRateLimitMs != 500 {
		t.Errorf("FetchRateLimitMs = %d, want 500", cfg.FetchRateLimitMs)
	}
	if cfg.OpenAIModel != "gpt-4o-mini" {
		t.Errorf("OpenAIModel = %q, want %q", cfg.OpenAIModel, "gpt-4o-mini")
	}
	if cfg.EmbeddingModel != "text-embedding-3-small" {
		t.Errorf("EmbeddingModel = %q, want %q", cfg.EmbeddingModel, "text-embedding-3-small")
	}
	if cfg.QdrantCollection != "equinox" {
		t.Errorf("QdrantCollection = %q, want %q", cfg.QdrantCollection, "equinox")
	}
	if cfg.NewsEnabled != false {
		t.Errorf("NewsEnabled = %v, want false", cfg.NewsEnabled)
	}
	if cfg.NewsMaxArticles != 5 {
		t.Errorf("NewsMaxArticles = %d, want 5", cfg.NewsMaxArticles)
	}
}

func TestValidateWeightsSum(t *testing.T) {
	t.Setenv("PRICE_WEIGHT", "0.50")
	t.Setenv("LIQUIDITY_WEIGHT", "0.50")
	t.Setenv("SPREAD_WEIGHT", "0.50")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when weights sum to 1.5, got nil")
	}
}

func TestValidateThresholdOrder(t *testing.T) {
	// MatchThreshold < ProbableMatchThreshold should fail
	t.Setenv("MATCH_THRESHOLD", "0.30")
	t.Setenv("PROBABLE_MATCH_THRESHOLD", "0.40")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when MatchThreshold < ProbableMatchThreshold, got nil")
	}
}

func TestValidateThresholdRange(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"above 1", "1.5"},
		{"below 0", "-0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MATCH_THRESHOLD", tt.value)
			// Ensure probable threshold is valid and below match threshold
			t.Setenv("PROBABLE_MATCH_THRESHOLD", "0.01")

			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for MATCH_THRESHOLD=%s, got nil", tt.value)
			}
		})
	}
}

func TestValidateMaxDateDelta(t *testing.T) {
	t.Setenv("MAX_DATE_DELTA_DAYS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when MAX_DATE_DELTA_DAYS=0, got nil")
	}
}

func TestValidateHTTPTimeout(t *testing.T) {
	t.Setenv("HTTP_TIMEOUT_SECONDS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when HTTP_TIMEOUT_SECONDS=0, got nil")
	}
}

func TestValidateNegativeWeights(t *testing.T) {
	// Negative weight with others compensating to sum=1.0
	t.Setenv("PRICE_WEIGHT", "-0.5")
	t.Setenv("LIQUIDITY_WEIGHT", "1.0")
	t.Setenv("SPREAD_WEIGHT", "0.5")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for negative PRICE_WEIGHT, got nil")
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("MATCH_THRESHOLD", "0.80")
	t.Setenv("PROBABLE_MATCH_THRESHOLD", "0.50")
	t.Setenv("MAX_DATE_DELTA_DAYS", "180")
	t.Setenv("DEFAULT_ORDER_SIZE", "5000")
	t.Setenv("HTTP_TIMEOUT_SECONDS", "60")
	t.Setenv("FETCH_STRATEGY", "broad")
	t.Setenv("MARKETS_PER_CATEGORY", "100")
	t.Setenv("OPEN_AI_MODEL", "gpt-4o")
	t.Setenv("NEWS_ENABLED", "true")
	t.Setenv("NEWS_MAX_ARTICLES", "10")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.MatchThreshold != 0.80 {
		t.Errorf("MatchThreshold = %f, want 0.80", cfg.MatchThreshold)
	}
	if cfg.ProbableMatchThreshold != 0.50 {
		t.Errorf("ProbableMatchThreshold = %f, want 0.50", cfg.ProbableMatchThreshold)
	}
	if cfg.MaxDateDeltaDays != 180 {
		t.Errorf("MaxDateDeltaDays = %d, want 180", cfg.MaxDateDeltaDays)
	}
	if cfg.DefaultOrderSize != 5000.0 {
		t.Errorf("DefaultOrderSize = %f, want 5000.0", cfg.DefaultOrderSize)
	}
	if cfg.HTTPTimeout != 60*time.Second {
		t.Errorf("HTTPTimeout = %v, want 60s", cfg.HTTPTimeout)
	}
	if cfg.FetchStrategy != "broad" {
		t.Errorf("FetchStrategy = %q, want %q", cfg.FetchStrategy, "broad")
	}
	if cfg.MarketsPerCategory != 100 {
		t.Errorf("MarketsPerCategory = %d, want 100", cfg.MarketsPerCategory)
	}
	if cfg.OpenAIModel != "gpt-4o" {
		t.Errorf("OpenAIModel = %q, want %q", cfg.OpenAIModel, "gpt-4o")
	}
	if cfg.NewsEnabled != true {
		t.Errorf("NewsEnabled = %v, want true", cfg.NewsEnabled)
	}
	if cfg.NewsMaxArticles != 10 {
		t.Errorf("NewsMaxArticles = %d, want 10", cfg.NewsMaxArticles)
	}
}
