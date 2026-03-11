// Package normalizer transforms venue-specific RawMarkets into CanonicalMarkets.
// It is the only package allowed to contain venue-specific parsing logic.
package normalizer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/venues"

	"github.com/google/uuid"
)

// Normalizer converts RawMarkets into CanonicalMarkets.
type Normalizer struct {
	cfg *config.Config
}

// New creates a Normalizer.
func New(cfg *config.Config) *Normalizer {
	return &Normalizer{cfg: cfg}
}

// Normalize converts a batch of RawMarkets from a single venue into CanonicalMarkets.
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
	Image         string `json:"image"` // event-level image URL (injected by client)
	// public-search fields (not present in /markets endpoint)
	BestBid       float64 `json:"bestBid"`
	BestAsk       float64 `json:"bestAsk"`
	Spread        float64 `json:"spread"`
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

	// public-search has no "id" field, use slug as fallback
	marketID := raw.ID
	if marketID == "" {
		marketID = raw.Slug
	}

	m := &models.CanonicalMarket{
		ID:               uuid.NewString(),
		VenueID:          models.VenuePolymarket,
		VenueMarketID:    marketID,
		VenueEventTicker: eventSlug,
		VenueSlug:        raw.Slug,
		Title:            raw.Question,
		Description:   raw.Description,
		Category:      models.NormalizeCategory(strings.ToLower(raw.Category)),
		ImageURL:      raw.Image,
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
		}
	}

	// public-search provides bestBid/bestAsk/spread directly — use as fallback or override
	if raw.BestBid > 0 || raw.BestAsk > 0 {
		m.Spread = raw.Spread
		// If outcomePrices didn't give us prices, use bestBid/bestAsk midpoint
		if m.YesPrice == 0 && raw.BestBid > 0 {
			m.YesPrice = (raw.BestBid + raw.BestAsk) / 2
			m.NoPrice = 1 - m.YesPrice
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
	ImageURLLight string  `json:"image_url_light_mode"`
	ImageURLDark  string  `json:"image_url_dark_mode"`
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

	// Use the fetch category if provided (from category-bucketed fetch),
	// otherwise fall back to "other"
	cat := "other"
	if r.FetchCategory != "" {
		cat = models.NormalizeCategory(strings.ToLower(r.FetchCategory))
	}

	m := &models.CanonicalMarket{
		ID:                uuid.NewString(),
		VenueID:           models.VenueKalshi,
		VenueMarketID:     raw.Ticker,
		VenueEventTicker:  raw.EventTicker,
		VenueSeriesTicker: raw.SeriesTicker,
		VenueEventTitle:   raw.EventTitle,
		Title:             kalshiCanonicalTitle(raw.EventTitle, raw.Subtitle),
		Description:   raw.Subtitle,
		Category:      cat,
		YesPrice:      yesMid,
		NoPrice:       noMid,
		Spread:        yesSpread,
		Volume24h:     raw.Volume24h,
		OpenInterest:  raw.OpenInterest,
		Liquidity:     estimateKalshiLiquidity(raw),
		Status:        models.StatusActive,
		ImageURL:      raw.ImageURLLight,
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

// kalshiCanonicalTitle builds a full descriptive title for a Kalshi market.
// Combines the event title with the market subtitle (e.g. "Champions League Winner — Arsenal").
func kalshiCanonicalTitle(eventTitle, subtitle string) string {
	event := strings.TrimSpace(eventTitle)
	sub := strings.TrimSpace(subtitle)

	if event == "" {
		return sub
	}
	if sub == "" {
		return event
	}

	return event + " — " + sub
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
