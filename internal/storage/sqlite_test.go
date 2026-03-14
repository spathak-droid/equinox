package storage

import (
	"os"
	"testing"
	"time"

	"github.com/equinox/internal/models"
)

func TestStoreBasics(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	resDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	markets := []*models.CanonicalMarket{
		{
			VenueID:        models.VenuePolymarket,
			VenueMarketID:  "poly-btc-100k",
			Title:          "Will Bitcoin reach $100,000 by June 2026?",
			Category:       "crypto",
			YesPrice:       0.65,
			NoPrice:        0.35,
			Volume24h:      50000,
			Liquidity:      10000,
			Status:         models.StatusActive,
			ResolutionDate: &resDate,
		},
		{
			VenueID:       models.VenueKalshi,
			VenueMarketID: "kalshi-btc-100k",
			Title:         "Bitcoin to exceed $100,000 on June 30, 2026",
			Category:      "crypto",
			YesPrice:      0.62,
			NoPrice:       0.38,
			Volume24h:     30000,
			Liquidity:     8000,
			Status:        models.StatusActive,
		},
		{
			VenueID:       models.VenuePolymarket,
			VenueMarketID: "poly-trump-2028",
			Title:         "Will Trump win the 2028 presidential election?",
			Category:      "politics",
			YesPrice:      0.30,
			NoPrice:       0.70,
			Volume24h:     100000,
			Liquidity:     50000,
			Status:        models.StatusActive,
		},
	}

	// Test UpsertMarkets
	inserted, _, err := store.UpsertMarkets(markets)
	if err != nil {
		t.Fatalf("UpsertMarkets: %v", err)
	}
	if inserted != 3 {
		t.Errorf("expected 3 inserted, got %d", inserted)
	}

	// Test Stats
	stats, err := store.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.Total != 3 {
		t.Errorf("expected total=3, got %d", stats.Total)
	}
	if stats.ByVenue["polymarket"] != 2 {
		t.Errorf("expected polymarket=2, got %d", stats.ByVenue["polymarket"])
	}

	// Rebuild FTS index (done separately from upsert for performance)
	if err := store.RebuildFTS(); err != nil {
		t.Fatalf("RebuildFTS: %v", err)
	}

	// Test FTS search - should find Bitcoin markets
	results, err := store.SearchByTitle("Bitcoin", "", 10)
	if err != nil {
		t.Fatalf("SearchByTitle: %v", err)
	}
	if len(results) < 1 {
		// Debug: check FTS table content
		var ftsCount int
		store.db.QueryRow(`SELECT COUNT(*) FROM markets_fts`).Scan(&ftsCount)
		t.Fatalf("expected at least 1 Bitcoin result, got %d (FTS rows: %d)", len(results), ftsCount)
	}

	// Test FTS search with venue exclusion
	results, err = store.SearchByTitle("Bitcoin", "polymarket", 10)
	if err != nil {
		t.Fatalf("SearchByTitle: %v", err)
	}
	for _, r := range results {
		if r.VenueID == models.VenuePolymarket {
			t.Error("expected no polymarket results when excluded")
		}
	}

	// Test GetAllMarkets
	all, err := store.GetAllMarkets("")
	if err != nil {
		t.Fatalf("GetAllMarkets: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 markets, got %d", len(all))
	}

	// Test GetAllMarkets with venue filter
	kalshiOnly, err := store.GetAllMarkets("kalshi")
	if err != nil {
		t.Fatalf("GetAllMarkets(kalshi): %v", err)
	}
	if len(kalshiOnly) != 1 {
		t.Errorf("expected 1 kalshi market, got %d", len(kalshiOnly))
	}

	// Test upsert (update existing)
	markets[0].YesPrice = 0.70
	inserted2, _, err := store.UpsertMarkets(markets[:1])
	if err != nil {
		t.Fatalf("UpsertMarkets (update): %v", err)
	}
	if inserted2 != 1 {
		t.Errorf("expected 1 upserted, got %d", inserted2)
	}

	// Verify update took effect
	updated, err := store.GetAllMarkets("polymarket")
	if err != nil {
		t.Fatalf("GetAllMarkets: %v", err)
	}
	for _, m := range updated {
		if m.VenueMarketID == "poly-btc-100k" && m.YesPrice != 0.70 {
			t.Errorf("expected YesPrice=0.70 after update, got %.2f", m.YesPrice)
		}
	}

	// Test PurgeStale
	purged, err := store.PurgeStale(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeStale: %v", err)
	}
	if purged != 3 {
		t.Errorf("expected 3 purged, got %d", purged)
	}

	// Cleanup
	os.Remove(dbPath)
}

func TestSanitizeFTSQuery(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Bitcoin $100k?", "Bitcoin 100k"},
		{"Will Trump win?", "Will Trump win"},
		{"", ""},
		{"  spaces  ", "spaces"},
		{"a-b-c", "a b c"},
		{"normal query", "normal query"},
	}

	for _, tt := range tests {
		got := sanitizeFTSQuery(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeFTSQuery(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
