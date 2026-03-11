// Package venues defines the Venue interface that all venue-specific integrations implement.
// This is the boundary layer — nothing outside of the venues package should know about
// venue-specific API shapes, field names, or pricing formats.
package venues

import (
	"context"
	"encoding/json"

	"github.com/equinox/internal/models"
)

// RawMarket holds the unprocessed response from a venue API.
// It is an intermediate type — the normalizer converts it to a CanonicalMarket.
type RawMarket struct {
	VenueID       models.VenueID
	VenueMarketID string
	FetchCategory string          // category used to fetch this market (set by BucketedFetcher)
	Payload       json.RawMessage // verbatim JSON from venue
}

// Venue is the interface that every venue integration must implement.
// Implementations live in sub-packages (venues/polymarket, venues/kalshi, etc.)
// and are registered in main.go.
//
// Design principle: Venue implementations are responsible ONLY for fetching raw data.
// All transformation to CanonicalMarket happens in the normalizer package.
// This separation means adding a new venue requires only:
//  1. Implementing this interface
//  2. Writing a normalizer for its schema
//  3. Registering it in main.go
type Venue interface {
	// ID returns the stable identifier for this venue.
	ID() models.VenueID

	// FetchMarkets retrieves all currently active markets from the venue.
	// Implementations should handle pagination internally and return a flat slice.
	FetchMarkets(ctx context.Context) ([]*RawMarket, error)
}

// SearchableVenue extends Venue with text-based market search.
// Venues that implement this interface enable query-based cross-search matching,
// which dramatically reduces the search space compared to brute-force O(n²) comparison.
type SearchableVenue interface {
	Venue

	// FetchMarketsByQuery searches for markets matching the given text query.
	// The query is typically a market title from another venue.
	FetchMarketsByQuery(ctx context.Context, query string) ([]*RawMarket, error)
}

// CategoryVenue extends Venue with category-based market fetching.
// Each venue maps a normalized category name to its own API parameter
// (e.g., Polymarket uses tag slugs, Kalshi uses series categories).
type CategoryVenue interface {
	Venue

	// FetchMarketsByCategory returns active markets for a given normalized category.
	// The category is a lowercase string like "politics", "crypto", "economics", "sports".
	FetchMarketsByCategory(ctx context.Context, category string) ([]*RawMarket, error)
}

// CategoryVenueWithLimit extends CategoryVenue with configurable per-call limits.
type CategoryVenueWithLimit interface {
	CategoryVenue

	// FetchMarketsByCategoryWithLimit returns active markets for a category with a per-call limit.
	FetchMarketsByCategoryWithLimit(ctx context.Context, category string, limit int) ([]*RawMarket, error)
}
