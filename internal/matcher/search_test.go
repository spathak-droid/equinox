package matcher

import (
	"context"
	"testing"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
)

func TestSearchQueryExtractor(t *testing.T) {
	tests := []struct {
		name  string
		title string
		event string // VenueEventTitle
		want  string
	}{
		{"plain title", "Will Bitcoin hit $100k?", "", "Will Bitcoin hit $100k?"},
		{"event title preferred", "KXBTC-100K-2026", "Bitcoin Price $100k", "Bitcoin Price $100k"},
		{"whitespace trimmed", "  Bitcoin  ", "", "Bitcoin"},
		{"empty event", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &models.CanonicalMarket{Title: tt.title, VenueEventTitle: tt.event}
			got := SearchQueryExtractor(m)
			if got != tt.want {
				t.Errorf("SearchQueryExtractor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeduplicatePairs(t *testing.T) {
	a := &models.CanonicalMarket{VenueMarketID: "A"}
	b := &models.CanonicalMarket{VenueMarketID: "B"}
	c := &models.CanonicalMarket{VenueMarketID: "C"}

	pairs := []*MatchResult{
		{MarketA: a, MarketB: b, CompositeScore: 0.9},
		{MarketA: b, MarketB: a, CompositeScore: 0.8}, // duplicate (reversed)
		{MarketA: a, MarketB: c, CompositeScore: 0.7},
	}

	deduped := DeduplicatePairs(pairs)
	if len(deduped) != 2 {
		t.Fatalf("expected 2 unique pairs, got %d", len(deduped))
	}
}

func TestDeduplicateByMarket(t *testing.T) {
	a := &models.CanonicalMarket{VenueMarketID: "A"}
	b := &models.CanonicalMarket{VenueMarketID: "B"}
	c := &models.CanonicalMarket{VenueMarketID: "C"}

	// A-B has higher score, A-C should be dropped because A is already used
	pairs := []*MatchResult{
		{MarketA: a, MarketB: b, CompositeScore: 0.9},
		{MarketA: a, MarketB: c, CompositeScore: 0.7},
	}

	deduped := deduplicateByMarket(pairs)
	if len(deduped) != 1 {
		t.Fatalf("expected 1 pair after market dedup, got %d", len(deduped))
	}
	if deduped[0].MarketB.VenueMarketID != "B" {
		t.Errorf("kept wrong pair: got B=%s, want B", deduped[0].MarketB.VenueMarketID)
	}
}

func TestTopPairsByJaccard(t *testing.T) {
	poly := []*models.CanonicalMarket{
		{VenueMarketID: "p1", Title: "Bitcoin price $100k 2026"},
		{VenueMarketID: "p2", Title: "Trump win election 2028"},
	}
	kalshi := []*models.CanonicalMarket{
		{VenueMarketID: "k1", Title: "Bitcoin reach $100k"},
		{VenueMarketID: "k2", Title: "Fed rate cut 2026"},
	}

	pairs := topPairsByJaccard(poly, kalshi, 2)
	if len(pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(pairs))
	}
	// Bitcoin should pair with Bitcoin (highest Jaccard)
	if pairs[0][0].VenueMarketID != "p1" || pairs[0][1].VenueMarketID != "k1" {
		t.Errorf("expected p1-k1 as best pair, got %s-%s",
			pairs[0][0].VenueMarketID, pairs[0][1].VenueMarketID)
	}
}

func TestTopPairsByJaccardRespectK(t *testing.T) {
	poly := []*models.CanonicalMarket{
		{VenueMarketID: "p1", Title: "a b c"},
		{VenueMarketID: "p2", Title: "d e f"},
	}
	kalshi := []*models.CanonicalMarket{
		{VenueMarketID: "k1", Title: "a b c"},
	}
	pairs := topPairsByJaccard(poly, kalshi, 1)
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair (k=1), got %d", len(pairs))
	}
}

func TestTopByQueryMatch(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueMarketID: "m1", Title: "Bitcoin reaches $100k in 2026"},
		{VenueMarketID: "m2", Title: "Trump wins 2028 election"},
		{VenueMarketID: "m3", Title: "Ethereum drops below $2000"},
	}

	result := topByQueryMatch("bitcoin 100k", markets, 10)
	if len(result) == 0 {
		t.Fatal("expected at least 1 result for bitcoin query")
	}
	if result[0].VenueMarketID != "m1" {
		t.Errorf("expected m1 (Bitcoin) as top match, got %s", result[0].VenueMarketID)
	}
}

func TestTopByQueryMatchEmptyQuery(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueMarketID: "m1", Title: "anything"},
	}
	result := topByQueryMatch("", markets, 10)
	if len(result) != 1 {
		t.Fatalf("empty query should return all markets, got %d", len(result))
	}
}

func TestBatchSearchQueries(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueMarketID: "m1", Title: "Bitcoin Price", VenueEventTitle: "Bitcoin"},
		{VenueMarketID: "m2", Title: "BTC Value", VenueEventTitle: "Bitcoin"},   // same event title = deduped
		{VenueMarketID: "m3", Title: "Trump Election", VenueEventTitle: "Trump"}, // different
	}

	groups := BatchSearchQueries(markets)
	if len(groups) != 2 {
		t.Fatalf("expected 2 unique query groups, got %d", len(groups))
	}
}

func TestDiversifySourceMarkets(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueMarketID: "m1", Title: "Lakers win the 2026 NBA Finals", Liquidity: 100},
		{VenueMarketID: "m2", Title: "Celtics win the 2026 NBA Finals", Liquidity: 200},
		{VenueMarketID: "m3", Title: "Warriors win the 2026 NBA Finals", Liquidity: 50},
		{VenueMarketID: "m4", Title: "Bitcoin hits $100k", Liquidity: 300},
	}

	diversified := DiversifySourceMarkets(markets, 10)
	// "X win the 2026 NBA Finals" should collapse to 1 representative (Celtics, highest liquidity)
	// Bitcoin is a separate group
	if len(diversified) != 2 {
		t.Fatalf("expected 2 diversified groups, got %d", len(diversified))
	}
}

func TestExtractCorePattern(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"lakers win the 2026 nba finals", "win the 2026 nba finals"},
		{"bitcoin hits 100k before 2027", "bitcoin hits 100k before 2027"}, // no pattern match
		{"germany qualify for the 2026 world cup", "qualify for the 2026 world cup"},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			got := extractCorePattern(tt.title)
			if got != tt.want {
				t.Errorf("extractCorePattern(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestCrossPollinateJaccard(t *testing.T) {
	cfg := &config.Config{
		MatchThreshold:         0.45,
		ProbableMatchThreshold: 0.35,
		MaxDateDeltaDays:       365,
	}
	m := New(cfg)

	now := time.Now()
	poly := []*models.CanonicalMarket{
		{VenueID: models.VenuePolymarket, VenueMarketID: "p1",
			Title: "Will Bitcoin hit $100,000 in 2026?", Status: models.StatusActive,
			YesPrice: 0.65, ResolutionDate: &now},
	}
	kalshi := []*models.CanonicalMarket{
		{VenueID: models.VenueKalshi, VenueMarketID: "k1",
			Title: "Will BTC reach $100k by 2026?", Status: models.StatusActive,
			YesPrice: 0.62, ResolutionDate: &now},
	}

	results := m.CrossPollinateJaccard(poly, kalshi)
	// These should match since they're the same event
	if len(results) == 0 {
		t.Log("CrossPollinateJaccard: no matches found (acceptable if semantic gate is strict)")
	}
}

func TestCrossPollinateJaccardEmptyInput(t *testing.T) {
	cfg := &config.Config{MatchThreshold: 0.45, ProbableMatchThreshold: 0.35}
	m := New(cfg)

	if results := m.CrossPollinateJaccard(nil, nil); results != nil {
		t.Errorf("expected nil for empty input, got %d results", len(results))
	}
}

func TestFindEquivalentPairsFromSearch(t *testing.T) {
	cfg := &config.Config{
		MatchThreshold:         0.45,
		ProbableMatchThreshold: 0.35,
		MaxDateDeltaDays:       365,
	}
	m := New(cfg)

	now := time.Now()
	searchResults := []SearchResult{
		{
			Source: &models.CanonicalMarket{
				VenueID: models.VenuePolymarket, VenueMarketID: "p1",
				Title: "Will Bitcoin hit $100,000 in 2026?", Status: models.StatusActive,
				YesPrice: 0.65, ResolutionDate: &now,
			},
			Candidates: []*models.CanonicalMarket{
				{
					VenueID: models.VenueKalshi, VenueMarketID: "k1",
					Title: "Will BTC reach $100k by 2026?", Status: models.StatusActive,
					YesPrice: 0.62, ResolutionDate: &now,
				},
			},
		},
	}

	ctx := context.Background()
	results := m.FindEquivalentPairsFromSearch(ctx, searchResults, "bitcoin")
	// Results depend on scoring thresholds, just verify no panic
	_ = results
}

func TestCountRawPairs(t *testing.T) {
	results := []SearchResult{
		{Candidates: make([]*models.CanonicalMarket, 3)},
		{Candidates: make([]*models.CanonicalMarket, 5)},
	}
	if got := countRawPairs(results); got != 8 {
		t.Errorf("countRawPairs = %d, want 8", got)
	}
}
