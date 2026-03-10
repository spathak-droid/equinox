// Package models defines the canonical internal representation of a prediction market.
// All venue-specific schemas are translated into this form before any downstream
// processing (matching, routing, storage). This ensures that the matcher and router
// never contain venue-specific assumptions.
package models

import (
	"encoding/json"
	"time"
)

// MarketStatus represents the lifecycle state of a market.
type MarketStatus string

const (
	StatusActive   MarketStatus = "active"
	StatusClosed   MarketStatus = "closed"
	StatusResolved MarketStatus = "resolved"
	StatusUnknown  MarketStatus = "unknown"
)

// VenueID is a stable identifier for a prediction market venue.
type VenueID string

const (
	VenuePolymarket VenueID = "polymarket"
	VenueKalshi     VenueID = "kalshi"
)

// CanonicalMarket is the unified internal representation of a prediction market.
//
// Design rationale:
//   - All prices are normalized to [0.0, 1.0] implying probability.
//     Polymarket uses cents (0–100), Kalshi uses cents too but with different field names.
//     By converting both to float [0,1] here, the router never needs to know the source format.
//   - ResolutionDate is a pointer to handle markets with no defined expiry (e.g. Manifold).
//   - TitleEmbedding is populated lazily by the normalizer only when an AI key is present.
//   - RawPayload is retained verbatim for debugging and auditability. It is never read
//     by the matcher or router.
type CanonicalMarket struct {
	// --- Identity ---
	ID            string  `json:"id"`             // Equinox-generated UUID (stable per venue market)
	VenueID       VenueID `json:"venue_id"`        // Source venue
	VenueMarketID string  `json:"venue_market_id"` // Venue's own opaque market identifier
	VenueEventTicker  string `json:"venue_event_ticker,omitempty"`  // API event identifier, when available
	VenueSeriesTicker string `json:"venue_series_ticker,omitempty"` // Kalshi series ticker for URL construction
	VenueEventTitle   string `json:"venue_event_title,omitempty"`   // Event-level title for URL slug construction
	VenueSlug         string `json:"venue_slug,omitempty"`

	// --- Human-readable description ---
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"` // Normalized broad category (see NormalizeCategory)
	Tags        []string `json:"tags,omitempty"`

	// --- Resolution ---
	// ResolutionDate is nil when the venue does not specify one.
	// Assumption: markets without a resolution date are not date-gated and may still match on content signals.
	ResolutionDate     *time.Time `json:"resolution_date,omitempty"`
	ResolutionCriteria string     `json:"resolution_criteria,omitempty"`

	// --- Pricing (all normalized to [0.0, 1.0] probability) ---
	YesPrice float64 `json:"yes_price"` // Probability of YES outcome
	NoPrice  float64 `json:"no_price"`  // Probability of NO outcome (often 1 - YesPrice)
	Spread   float64 `json:"spread"`    // Ask - Bid; 0 if not available

	// --- Liquidity (all in USD) ---
	Volume24h    float64 `json:"volume_24h,omitempty"`
	OpenInterest float64 `json:"open_interest,omitempty"`
	Liquidity    float64 `json:"liquidity,omitempty"` // Available depth at current price

	// --- Lifecycle ---
	Status    MarketStatus `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`

	// --- AI features ---
	// TitleEmbedding holds an OpenAI text-embedding-3-small vector (1536 dims).
	// Populated by the Normalizer when OPENAI_API_KEY is set.
	// If nil, the matcher falls back to rule-based scoring only.
	TitleEmbedding []float32 `json:"title_embedding,omitempty"`

	// --- Audit trail ---
	// RawPayload is the unmodified JSON from the venue API.
	// Never used by matching or routing logic — only for debugging.
	RawPayload json.RawMessage `json:"raw_payload,omitempty"`
}

// EmbeddingText returns the string that should be embedded for this market.
// We use only the title so that cross-venue description differences don't
// pollute the embedding similarity signal.
func (m *CanonicalMarket) EmbeddingText() string {
	return m.Title
}

// HasResolutionDate returns true when a resolution date is known.
func (m *CanonicalMarket) HasResolutionDate() bool {
	return m.ResolutionDate != nil
}

// NormalizeCategory maps venue-specific category strings to a small controlled vocabulary.
// This is intentionally lossy — we care about broad groupings for pre-filtering, not taxonomy.
//
// Assumption: categories that don't map to a known bucket are assigned "other".
// This is acceptable because category matching is a soft filter, not a hard exclusion.
func NormalizeCategory(raw string) string {
	mapping := map[string]string{
		"politics":    "politics",
		"election":    "politics",
		"government":  "politics",
		"economics":   "economics",
		"economy":     "economics",
		"finance":     "economics",
		"markets":     "economics",
		"crypto":      "crypto",
		"bitcoin":     "crypto",
		"ethereum":    "crypto",
		"sports":      "sports",
		"nba":         "sports",
		"nfl":         "sports",
		"soccer":      "sports",
		"science":     "science",
		"technology":  "technology",
		"tech":        "technology",
		"ai":          "technology",
		"geopolitics": "geopolitics",
		"world":       "geopolitics",
	}

	if normalized, ok := mapping[raw]; ok {
		return normalized
	}
	return "other"
}
