// Package fetcher implements category-bucketed parallel fetching from venues.
//
// Instead of calling FetchMarkets() (which returns whatever the venue's default
// ordering is — often sports), the BucketedFetcher systematically fetches markets
// by category from each venue in parallel. This:
//   - Eliminates the sports-bias problem (Kalshi's default returns NCAA first)
//   - Pre-partitions markets by topic, making matching cheaper
//   - Produces category-aligned datasets without query generation
package fetcher

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/equinox/internal/models"
	"github.com/equinox/internal/venues"
)

// Categories is the canonical set of categories both venues support.
var Categories = []string{
	"politics", "crypto", "economics", "sports",
	"science", "entertainment", "world",
}

// AdjacentCategories maps categories that often overlap across venues.
// Used for cross-bucket matching after within-bucket matching.
var AdjacentCategories = map[string][]string{
	"crypto":        {"economics"},
	"economics":     {"crypto", "politics"},
	"politics":      {"world"},
	"world":         {"politics"},
	"science":       {"technology"},
	"technology":    {"science"},
}

// CategoryBucket holds markets from all venues for a single category.
type CategoryBucket struct {
	Category string
	Markets  map[models.VenueID][]*venues.RawMarket
}

// TotalMarkets returns the total number of markets across all venues in this bucket.
func (b *CategoryBucket) TotalMarkets() int {
	total := 0
	for _, markets := range b.Markets {
		total += len(markets)
	}
	return total
}

// HasCrossVenue returns true if the bucket has markets from at least 2 venues.
func (b *CategoryBucket) HasCrossVenue() bool {
	count := 0
	for _, markets := range b.Markets {
		if len(markets) > 0 {
			count++
		}
	}
	return count >= 2
}

// FetchConfig controls the bucketed fetcher behavior.
type FetchConfig struct {
	MarketsPerCategory int           // max markets per category per venue
	Concurrency        int           // max parallel HTTP calls
	RateLimit          time.Duration // delay between calls to the same venue
}

// BucketedFetcher fetches markets from multiple venues organized by category.
type BucketedFetcher struct {
	venues []venues.CategoryVenue
	config FetchConfig
}

// New creates a BucketedFetcher.
func New(categoryVenues []venues.CategoryVenue, cfg FetchConfig) *BucketedFetcher {
	if cfg.MarketsPerCategory <= 0 {
		cfg.MarketsPerCategory = 50
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.RateLimit <= 0 {
		cfg.RateLimit = 500 * time.Millisecond
	}
	return &BucketedFetcher{
		venues: categoryVenues,
		config: cfg,
	}
}

// fetchResult is the result of a single category+venue fetch.
type fetchResult struct {
	category string
	venueID  models.VenueID
	markets  []*venues.RawMarket
	err      error
}

// FetchAll fetches markets from all venues for all categories in parallel.
// Returns a slice of CategoryBuckets (one per category with data).
func (f *BucketedFetcher) FetchAll(ctx context.Context) ([]CategoryBucket, error) {
	// Build work items: one per (category, venue) pair
	type workItem struct {
		category string
		venue    venues.CategoryVenue
	}
	var work []workItem
	for _, cat := range Categories {
		for _, v := range f.venues {
			work = append(work, workItem{category: cat, venue: v})
		}
	}

	fmt.Printf("[fetcher] Starting category-bucketed fetch: %d categories x %d venues = %d fetch groups\n",
		len(Categories), len(f.venues), len(work))

	// Run fetches with bounded concurrency and per-venue rate limiting
	results := make(chan fetchResult, len(work))
	sem := make(chan struct{}, f.config.Concurrency)

	// Per-venue rate limiters
	var mu sync.Mutex
	venueLastCall := map[models.VenueID]time.Time{}

	var wg sync.WaitGroup
	for _, w := range work {
		wg.Add(1)
		go func(wi workItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Per-venue rate limiting
			mu.Lock()
			last := venueLastCall[wi.venue.ID()]
			now := time.Now()
			if wait := f.config.RateLimit - now.Sub(last); wait > 0 {
				mu.Unlock()
				time.Sleep(wait)
			} else {
				mu.Unlock()
			}
			mu.Lock()
			venueLastCall[wi.venue.ID()] = time.Now()
			mu.Unlock()

			var markets []*venues.RawMarket
			var err error

			// Try limit-aware method first, fall back to basic
			if lv, ok := wi.venue.(venues.CategoryVenueWithLimit); ok {
				markets, err = lv.FetchMarketsByCategoryWithLimit(ctx, wi.category, f.config.MarketsPerCategory)
			} else {
				markets, err = wi.venue.FetchMarketsByCategory(ctx, wi.category)
			}

			// Set FetchCategory on all results
			if err == nil {
				for _, m := range markets {
					m.FetchCategory = wi.category
				}
			}

			results <- fetchResult{
				category: wi.category,
				venueID:  wi.venue.ID(),
				markets:  markets,
				err:      err,
			}
		}(w)
	}

	// Close results channel when all work is done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results into buckets
	bucketMap := map[string]*CategoryBucket{}
	totalMarkets := 0
	errors := 0

	for r := range results {
		if r.err != nil {
			fmt.Printf("[fetcher] WARNING: %s/%s fetch failed: %v\n", r.venueID, r.category, r.err)
			errors++
			continue
		}

		bucket, ok := bucketMap[r.category]
		if !ok {
			bucket = &CategoryBucket{
				Category: r.category,
				Markets:  map[models.VenueID][]*venues.RawMarket{},
			}
			bucketMap[r.category] = bucket
		}
		bucket.Markets[r.venueID] = r.markets
		totalMarkets += len(r.markets)
	}

	// Convert to slice, skipping empty buckets
	var buckets []CategoryBucket
	for _, cat := range Categories {
		if bucket, ok := bucketMap[cat]; ok && bucket.TotalMarkets() > 0 {
			buckets = append(buckets, *bucket)
		}
	}

	fmt.Printf("[fetcher] Fetch complete: %d buckets, %d total markets, %d errors\n",
		len(buckets), totalMarkets, errors)

	// Log per-bucket breakdown
	for _, b := range buckets {
		parts := []string{}
		for vid, markets := range b.Markets {
			parts = append(parts, fmt.Sprintf("%s=%d", vid, len(markets)))
		}
		crossVenue := ""
		if b.HasCrossVenue() {
			crossVenue = " [cross-venue]"
		}
		fmt.Printf("[fetcher]   %-15s %s%s\n", b.Category+":", joinStrings(parts), crossVenue)
	}

	return buckets, nil
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
