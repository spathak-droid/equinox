// Package normalizer transforms venue-specific RawMarkets into CanonicalMarkets.
// It is the only package allowed to contain venue-specific parsing logic.
//
// The normalizer also optionally enriches canonical markets with AI embeddings
// by calling the OpenAI Embeddings API. When no API key is configured, this step
// is silently skipped and the matcher falls back to rule-based scoring.
// Optional embedding cache can persist vectors by normalized title text to avoid
// repeat calls across repeated runs.
package normalizer

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/venues"

	"github.com/google/uuid"
)

// Normalizer converts RawMarkets into CanonicalMarkets.
type Normalizer struct {
	cfg    *config.Config
	openai *openai.Client // nil when no API key is set

	embeddingCache *embeddingCache
}

// New creates a Normalizer. If cfg.OpenAIAPIKey is empty, embedding enrichment is disabled.
func New(cfg *config.Config) *Normalizer {
	n := &Normalizer{cfg: cfg}
	if cfg.OpenAIAPIKey != "" {
		openaiCfg := openai.DefaultConfig(cfg.OpenAIAPIKey)
		openaiCfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
		n.openai = openai.NewClientWithConfig(openaiCfg)
	}
	if cfg.EmbeddingCacheEnabled {
		n.embeddingCache = &embeddingCache{
			Entries: map[string][]float32{},
		}
		if err := n.loadEmbeddingCache(); err != nil {
			fmt.Printf("[normalizer] WARNING: embedding cache unavailable: %v — continuing without cache\n", err)
			n.embeddingCache = &embeddingCache{Entries: map[string][]float32{}}
		}
	}
	return n
}

// Normalize converts a batch of RawMarkets from a single venue into CanonicalMarkets,
// then optionally enriches them with embeddings in a single batched API call.
func (n *Normalizer) Normalize(ctx context.Context, raw []*venues.RawMarket) ([]*models.CanonicalMarket, error) {
	canonical := make([]*models.CanonicalMarket, 0, len(raw))

	for _, r := range raw {
		var m *models.CanonicalMarket
		var err error

		switch r.VenueID {
		case models.VenuePolymarket:
			m, err = normalizePolymarket(r)
		case models.VenueKalshi:
			m, err = normalizeKalshi(r)
		default:
			return nil, fmt.Errorf("normalizer: unknown venue %q", r.VenueID)
		}

		if err != nil {
			// Log and skip — imperfect data should not abort the entire pipeline.
			fmt.Printf("[normalizer] WARNING: skipping market %s/%s: %v\n",
				r.VenueID, r.VenueMarketID, err)
			continue
		}
		canonical = append(canonical, m)
	}

	// Enrich with embeddings if an OpenAI client is available.
	if n.openai != nil {
		if err := n.enrichWithEmbeddings(ctx, canonical); err != nil {
			// Embedding failure is non-fatal — log and continue without embeddings.
			fmt.Printf("[normalizer] WARNING: embedding enrichment failed: %v — falling back to rule-only matching\n", err)
		}
	}

	return canonical, nil
}

// ─── Polymarket normalizer ────────────────────────────────────────────────────

type polymarketRaw struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	Question      string `json:"question"`
	Description   string `json:"description"`
	EndDateISO    string `json:"endDateIso"`
	OutcomePrices string `json:"outcomePrices"` // JSON array string: "[\"0.62\",\"0.38\"]"
	Volume        string `json:"volume"`        // API returns string
	Volume24hr    float64 `json:"volume24hr"`
	Liquidity     string `json:"liquidity"`     // API returns string
	LiquidityNum  float64 `json:"liquidityNum"`
	Category      string `json:"category"`
	Tags          []struct {
		Label string `json:"label"`
	} `json:"tags"`
	Events []struct {
		Slug string `json:"slug"`
	} `json:"events"`
}

func normalizePolymarket(r *venues.RawMarket) (*models.CanonicalMarket, error) {
	var raw polymarketRaw
	if err := json.Unmarshal(r.Payload, &raw); err != nil {
		return nil, fmt.Errorf("parsing polymarket payload: %w", err)
	}

	// Extract event slug for URL construction (Polymarket uses /event/<event-slug>/<market-slug>)
	var eventSlug string
	if len(raw.Events) > 0 && raw.Events[0].Slug != "" {
		eventSlug = raw.Events[0].Slug
	}

	m := &models.CanonicalMarket{
		ID:               uuid.NewString(),
		VenueID:          models.VenuePolymarket,
		VenueMarketID:    raw.ID,
		VenueEventTicker: eventSlug,
		VenueSlug:        raw.Slug,
		Title:            raw.Question,
		Description:   raw.Description,
		Category:      models.NormalizeCategory(strings.ToLower(raw.Category)),
		Volume24h:     raw.Volume24hr,
		Liquidity:     raw.LiquidityNum,
		Status:        models.StatusActive,
		UpdatedAt:     time.Now(),
		CreatedAt:     time.Now(),
		RawPayload:    r.Payload,
	}

	// Tags
	for _, t := range raw.Tags {
		m.Tags = append(m.Tags, strings.ToLower(t.Label))
	}

	// Resolution date — Polymarket uses ISO8601 strings; treat as optional
	if raw.EndDateISO != "" {
		t, err := time.Parse(time.RFC3339, raw.EndDateISO)
		if err != nil {
			t, err = time.Parse("2006-01-02T15:04:05Z", raw.EndDateISO)
		}
		if err != nil {
			t, err = time.Parse("2006-01-02", raw.EndDateISO)
		}
		if err == nil {
			m.ResolutionDate = &t
		} else {
			fmt.Printf("[normalizer/polymarket] WARNING: could not parse endDateIso %q for market %s\n",
				raw.EndDateISO, raw.ID)
		}
	}

	// Prices — OutcomePrices is a JSON-encoded string like "[\"0.62\",\"0.38\"]"
	// Index 0 = YES price (in [0,1]), Index 1 = NO price
	if raw.OutcomePrices != "" {
		var prices []string
		if err := json.Unmarshal([]byte(raw.OutcomePrices), &prices); err == nil && len(prices) >= 2 {
			yes, _ := strconv.ParseFloat(prices[0], 64)
			no, _ := strconv.ParseFloat(prices[1], 64)
			m.YesPrice = yes
			m.NoPrice = no
			// Spread is not directly available from Polymarket's summary endpoint
			// Assumption: we set spread to 0 and note it in docs as a known limitation
			m.Spread = 0
		}
	}

	return m, nil
}

// ─── Kalshi normalizer ────────────────────────────────────────────────────────

type kalshiRaw struct {
	Ticker        string  `json:"ticker"`
	EventTicker   string  `json:"event_ticker"`
	SeriesTicker  string  `json:"series_ticker"`  // injected by client from events cache
	EventTitle    string  `json:"event_title"`     // injected by client from events cache
	Title         string  `json:"title"`
	Subtitle      string  `json:"subtitle"`
	Status        string  `json:"status"`
	CloseTime     string  `json:"close_time"`
	YesBid        int     `json:"yes_bid"` // cents
	YesAsk        int     `json:"yes_ask"` // cents
	NoBid         int     `json:"no_bid"`
	NoAsk         int     `json:"no_ask"`
	Volume        float64 `json:"volume"`
	Volume24h     float64 `json:"volume_24h"`
	OpenInterest  float64 `json:"open_interest"`
	Liquidity     float64 `json:"liquidity"`
	RulesPrimary  string  `json:"rules_primary"`
	RulesSecondary string  `json:"rules_secondary"`
}

func normalizeKalshi(r *venues.RawMarket) (*models.CanonicalMarket, error) {
	var raw kalshiRaw
	if err := json.Unmarshal(r.Payload, &raw); err != nil {
		return nil, fmt.Errorf("parsing kalshi payload: %w", err)
	}

	// Kalshi prices in cents; normalize to [0.0, 1.0]
	yesMid := (float64(raw.YesBid) + float64(raw.YesAsk)) / 2.0 / 100.0
	noMid := (float64(raw.NoBid) + float64(raw.NoAsk)) / 2.0 / 100.0
	// Kalshi spread = ask - bid for the YES side
	yesSpread := float64(raw.YesAsk-raw.YesBid) / 100.0

	m := &models.CanonicalMarket{
		ID:                uuid.NewString(),
		VenueID:           models.VenueKalshi,
		VenueMarketID:     raw.Ticker,
		VenueEventTicker:  raw.EventTicker,
		VenueSeriesTicker: raw.SeriesTicker,
		VenueEventTitle:   raw.EventTitle,
		Title:             raw.Title,
		Description:   raw.Subtitle,
		Category:      "other",
		YesPrice:      yesMid,
		NoPrice:       noMid,
		Spread:        yesSpread,
		Volume24h:     raw.Volume24h,
		OpenInterest:  raw.OpenInterest,
		Liquidity:     estimateKalshiLiquidity(raw),
		Status:        models.StatusActive,
		UpdatedAt:     time.Now(),
		CreatedAt:     time.Now(),
		RawPayload:    r.Payload,
	}

	// Resolution date — Kalshi uses RFC3339
	if raw.CloseTime != "" {
		t, err := time.Parse(time.RFC3339, raw.CloseTime)
		if err == nil {
			m.ResolutionDate = &t
		} else {
			fmt.Printf("[normalizer/kalshi] WARNING: could not parse close_time %q for market %s\n",
				raw.CloseTime, raw.Ticker)
		}
	}

	// Fill description from rules if subtitle is empty
	if m.Description == "" && raw.RulesPrimary != "" {
		m.Description = raw.RulesPrimary
	} else if m.Description == "" && raw.RulesSecondary != "" {
		m.Description = raw.RulesSecondary
	}

	return m, nil
}

// estimateKalshiLiquidity derives a liquidity proxy from volume and spread
// because Kalshi's public API returns liquidity=0 for all markets.
// Formula: volume × (1 - spread), so high-volume tight-spread markets rank highest.
func estimateKalshiLiquidity(raw kalshiRaw) float64 {
	spread := float64(raw.YesAsk-raw.YesBid) / 100.0
	if spread < 0 {
		spread = 0
	}
	vol := raw.Volume
	if raw.Volume24h > vol {
		vol = raw.Volume24h
	}
	return vol * (1 - spread)
}

// ─── Embedding enrichment ─────────────────────────────────────────────────────

// embeddingBatchSize controls how many titles are sent in each OpenAI API call.
// Smaller batches are more resilient and show progress; 50 is a good balance.
const embeddingBatchSize = 50

// enrichWithEmbeddings calls the OpenAI Embeddings API in batches and attaches
// the resulting vectors. Cached embeddings are served from disk when available.
func (n *Normalizer) enrichWithEmbeddings(ctx context.Context, markets []*models.CanonicalMarket) error {
	if len(markets) == 0 {
		return nil
	}

	type pendingItem struct {
		index int // index into markets slice
		text  string
		key   string // cache key (empty if cache disabled)
	}

	// Resolve cache hits first
	var pending []pendingItem
	cacheHits := 0
	for i, market := range markets {
		text := market.EmbeddingText()
		if n.embeddingCache != nil {
			key := embeddingCacheKey(n.cfg.EmbeddingModel, text)
			if cached, ok := n.embeddingCache.Entries[key]; ok {
				market.TitleEmbedding = append([]float32(nil), cached...)
				cacheHits++
				continue
			}
			pending = append(pending, pendingItem{index: i, text: text, key: key})
		} else {
			pending = append(pending, pendingItem{index: i, text: text})
		}
	}

	if cacheHits > 0 {
		fmt.Printf("[normalizer] Embedding cache: %d/%d hits\n", cacheHits, len(markets))
	}

	if len(pending) == 0 {
		return nil
	}

	fmt.Printf("[normalizer] Fetching %d embeddings from OpenAI (batch size %d)...\n",
		len(pending), embeddingBatchSize)

	// Fire all embedding batches concurrently
	type embBatchResult struct {
		start int
		end   int
		resp  openai.EmbeddingResponse
		err   error
	}

	var batchSpecs []embBatchResult
	for start := 0; start < len(pending); start += embeddingBatchSize {
		end := start + embeddingBatchSize
		if end > len(pending) {
			end = len(pending)
		}
		batchSpecs = append(batchSpecs, embBatchResult{start: start, end: end})
	}

	ch := make(chan embBatchResult, len(batchSpecs))
	for _, bs := range batchSpecs {
		go func(start, end int) {
			batch := pending[start:end]
			texts := make([]string, len(batch))
			for i, p := range batch {
				texts[i] = p.text
			}
			fmt.Printf("[normalizer] Embedding batch %d-%d of %d...\n", start+1, end, len(pending))
			resp, err := n.openai.CreateEmbeddings(ctx, openai.EmbeddingRequestStrings{
				Model: openai.EmbeddingModel(n.cfg.EmbeddingModel),
				Input: texts,
			})
			ch <- embBatchResult{start: start, end: end, resp: resp, err: err}
		}(bs.start, bs.end)
	}

	updated := false
	for range batchSpecs {
		br := <-ch
		if br.err != nil {
			return fmt.Errorf("openai embeddings API (batch %d-%d): %w", br.start+1, br.end, br.err)
		}
		batch := pending[br.start:br.end]
		for _, emb := range br.resp.Data {
			if emb.Index >= len(batch) {
				continue
			}
			p := batch[emb.Index]
			markets[p.index].TitleEmbedding = emb.Embedding
			if n.embeddingCache != nil && p.key != "" {
				n.embeddingCache.Entries[p.key] = append([]float32(nil), emb.Embedding...)
				updated = true
			}
		}
	}

	if updated {
		if err := n.persistEmbeddingCache(); err != nil {
			fmt.Printf("[normalizer] WARNING: failed to write embedding cache: %v\n", err)
		} else {
			fmt.Printf("[normalizer] Embedding cache saved (%d entries)\n", len(n.embeddingCache.Entries))
		}
	}

	return nil
}

type embeddingCache struct {
	Entries map[string][]float32 `json:"entries"`
}

func embeddingCacheKey(model string, text string) string {
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(text)) + "|" + model))
	return hex.EncodeToString(sum[:])
}

func (n *Normalizer) loadEmbeddingCache() error {
	if n.cfg.EmbeddingCachePath == "" {
		return nil
	}
	raw, err := os.ReadFile(n.cfg.EmbeddingCachePath)
	if err != nil {
		// missing cache file is expected on first run
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading cache file %q: %w", n.cfg.EmbeddingCachePath, err)
	}

	var cache embeddingCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		return fmt.Errorf("parsing cache file %q: %w", n.cfg.EmbeddingCachePath, err)
	}
	if cache.Entries == nil {
		cache.Entries = map[string][]float32{}
	}
	n.embeddingCache = &cache
	return nil
}

func (n *Normalizer) persistEmbeddingCache() error {
	if n.cfg.EmbeddingCachePath == "" || n.embeddingCache == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(n.cfg.EmbeddingCachePath), 0o755); err != nil {
		return fmt.Errorf("creating cache directory for %q: %w", n.cfg.EmbeddingCachePath, err)
	}
	body, err := json.Marshal(n.embeddingCache)
	if err != nil {
		return fmt.Errorf("serializing embedding cache: %w", err)
	}
	if err := os.WriteFile(n.cfg.EmbeddingCachePath, body, 0o644); err != nil {
		return fmt.Errorf("writing cache file %q: %w", n.cfg.EmbeddingCachePath, err)
	}
	return nil
}

