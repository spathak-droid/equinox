package matcher

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
)

func TestBuildIndex(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueMarketID: "m1", VenueID: models.VenuePolymarket, Title: "Will Bitcoin hit $100k?"},
		{VenueMarketID: "m2", VenueID: models.VenueKalshi, Title: "Will Bitcoin reach $100k?"},
		{VenueMarketID: "m3", VenueID: models.VenuePolymarket, Title: "Will Trump win 2028?"},
	}

	idx := BuildIndex(markets)

	if len(idx.markets) != 3 {
		t.Fatalf("expected 3 indexed markets, got %d", len(idx.markets))
	}
	if len(idx.inverted) == 0 {
		t.Fatal("expected non-empty inverted index")
	}
	// "bitcoin" should appear for both m1 and m2
	if ids, ok := idx.inverted["bitcoin"]; ok {
		if len(ids) != 2 {
			t.Errorf("expected 2 markets for 'bitcoin', got %d", len(ids))
		}
	}
}

func TestBuildIndexIDFFiltering(t *testing.T) {
	// Create 600 markets so the IDF threshold = max(600/10, 50) = 60.
	// "will" appears in all 600 titles and should be filtered out.
	// "rarekeyword" appears in only 1 title and should survive.
	var markets []*models.CanonicalMarket
	for i := 0; i < 600; i++ {
		title := fmt.Sprintf("Will something happen scenario%d", i)
		if i == 0 {
			title = "Will rarekeyword happen scenario0"
		}
		markets = append(markets, &models.CanonicalMarket{
			VenueMarketID: fmt.Sprintf("m%d", i),
			VenueID:       models.VenuePolymarket,
			Title:         title,
		})
	}
	idx := BuildIndex(markets)

	// "will" appears in all 600 titles, threshold is 60 => should be filtered
	if _, found := idx.inverted["will"]; found {
		t.Error("expected 'will' to be filtered from inverted index (appears in all 600 markets)")
	}

	// "rarekeyword" appears in only 1 title => should NOT be filtered
	if ids, found := idx.inverted["rarekeyword"]; !found {
		t.Error("expected 'rarekeyword' to be in inverted index (appears in only 1 market)")
	} else if len(ids) != 1 {
		t.Errorf("expected 'rarekeyword' to map to 1 market, got %d", len(ids))
	}
}

func TestFindCandidatesCrossVenueOnly(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueMarketID: "p1", VenueID: models.VenuePolymarket, Title: "Bitcoin price 100k"},
		{VenueMarketID: "p2", VenueID: models.VenuePolymarket, Title: "Bitcoin value 100k"},
		{VenueMarketID: "k1", VenueID: models.VenueKalshi, Title: "Bitcoin reach 100k"},
	}

	idx := BuildIndex(markets)

	// p1 should find k1 (cross-venue) but NOT p2 (same venue)
	candidates := idx.FindCandidates(markets[0], 1)
	for _, c := range candidates {
		if c.VenueID == models.VenuePolymarket {
			t.Errorf("FindCandidates returned same-venue market %s", c.VenueMarketID)
		}
	}
}

func TestFindCandidatesMinSharedKeywords(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueMarketID: "p1", VenueID: models.VenuePolymarket, Title: "Bitcoin price prediction 100k target"},
		{VenueMarketID: "k1", VenueID: models.VenueKalshi, Title: "Bitcoin price 100k"},  // 3 shared
		{VenueMarketID: "k2", VenueID: models.VenueKalshi, Title: "Trump election 2028"}, // 0 shared
	}

	idx := BuildIndex(markets)
	candidates := idx.FindCandidates(markets[0], 2)

	found := false
	for _, c := range candidates {
		if c.VenueMarketID == "k1" {
			found = true
		}
		if c.VenueMarketID == "k2" {
			t.Error("k2 should not be a candidate (no shared keywords)")
		}
	}
	if !found {
		t.Error("k1 should be a candidate (shares multiple keywords)")
	}
}

func TestEntityOverlapScore(t *testing.T) {
	tests := []struct {
		name       string
		a, b       string
		minScore   float64
		maxScore   float64
	}{
		{"identical entities", "Trump Wins Election", "Trump Wins Election", 0.8, 1.0},
		{"no entities", "will the market rise", "prices go up", 0.4, 0.6}, // neutral 0.5
		{"disjoint entities", "Trump Wins", "Biden Loses", 0.0, 0.1},
		{"partial overlap", "Trump Biden Debate", "Trump Harris Debate", 0.2, 0.8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := entityOverlapScore(tt.a, tt.b)
			if score < tt.minScore || score > tt.maxScore {
				t.Errorf("entityOverlapScore(%q, %q) = %.3f, want [%.2f, %.2f]",
					tt.a, tt.b, score, tt.minScore, tt.maxScore)
			}
		})
	}
}

func TestDateProximityScore(t *testing.T) {
	d1 := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)  // 5 days apart
	d3 := time.Date(2027, 6, 15, 0, 0, 0, 0, time.UTC)  // 365 days apart

	tests := []struct {
		name   string
		a, b   *models.CanonicalMarket
		maxDays int
		want   float64
	}{
		{
			"within 30 days",
			&models.CanonicalMarket{ResolutionDate: &d1},
			&models.CanonicalMarket{ResolutionDate: &d2},
			365, 1.0,
		},
		{
			"at max delta",
			&models.CanonicalMarket{ResolutionDate: &d1},
			&models.CanonicalMarket{ResolutionDate: &d3},
			365, 0.0,
		},
		{
			"missing date A",
			&models.CanonicalMarket{},
			&models.CanonicalMarket{ResolutionDate: &d1},
			365, 0.5,
		},
		{
			"both missing",
			&models.CanonicalMarket{},
			&models.CanonicalMarket{},
			365, 0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dateProximityScore(tt.a, tt.b, tt.maxDays)
			if got != tt.want {
				t.Errorf("dateProximityScore = %.3f, want %.3f", got, tt.want)
			}
		})
	}
}

func TestPriceProximityScore(t *testing.T) {
	tests := []struct {
		name  string
		a, b  *models.CanonicalMarket
		want  float64
	}{
		{"identical prices", &models.CanonicalMarket{YesPrice: 0.65}, &models.CanonicalMarket{YesPrice: 0.65}, 1.0},
		{"both zero", &models.CanonicalMarket{YesPrice: 0}, &models.CanonicalMarket{YesPrice: 0}, 0.5},
		{"one zero", &models.CanonicalMarket{YesPrice: 0.5}, &models.CanonicalMarket{YesPrice: 0}, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := priceProximityScore(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("priceProximityScore = %.3f, want %.3f", got, tt.want)
			}
		})
	}
}

func TestCategoryBonus(t *testing.T) {
	tests := []struct {
		name   string
		catA   string
		catB   string
		expect float64
	}{
		{"same category", "crypto", "crypto", 0.15},
		{"different non-other", "crypto", "politics", -0.10},
		{"one is other", "crypto", "", 0.0},
		{"both other", "", "", 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &models.CanonicalMarket{Category: tt.catA}
			b := &models.CanonicalMarket{Category: tt.catB}
			got := categoryBonus(a, b)
			if got != tt.expect {
				t.Errorf("categoryBonus(%q, %q) = %.2f, want %.2f", tt.catA, tt.catB, got, tt.expect)
			}
		})
	}
}

func TestVoteBasedDisambiguation(t *testing.T) {
	tests := []struct {
		name   string
		result *MatchResult
		want   MatchConfidence
	}{
		{
			"high signals → MATCH",
			&MatchResult{
				MarketA: &models.CanonicalMarket{Category: "crypto", Title: "Bitcoin $100k"},
				MarketB: &models.CanonicalMarket{Category: "crypto", Title: "Bitcoin $100k"},
				FuzzyScore: 0.8, EntityOverlapScore: 0.9,
				DateProximityScore: 0.9, PriceProximityScore: 0.95,
			},
			ConfidenceMatch,
		},
		{
			"low signals → NO_MATCH",
			&MatchResult{
				MarketA: &models.CanonicalMarket{Category: "crypto", Title: "Bitcoin"},
				MarketB: &models.CanonicalMarket{Category: "politics", Title: "Trump"},
				FuzzyScore: 0.1, EntityOverlapScore: 0.0,
				DateProximityScore: 0.1, PriceProximityScore: 0.1,
			},
			ConfidenceNoMatch,
		},
		{
			"entity template mismatch → NO_MATCH",
			&MatchResult{
				MarketA: &models.CanonicalMarket{Title: "Spain Win World Cup"},
				MarketB: &models.CanonicalMarket{Title: "Germany Win World Cup"},
				FuzzyScore: 0.8, EntityOverlapScore: 0.1,
				DateProximityScore: 1.0, PriceProximityScore: 0.9,
			},
			ConfidenceNoMatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := voteBasedDisambiguation(tt.result)
			if got != tt.want {
				t.Errorf("voteBasedDisambiguation = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestFindSignatureMatchesCrossVenueOnly(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueID: models.VenuePolymarket, VenueMarketID: "p1",
			Title: "Will Bitcoin hit $100,000 in 2026?", Status: models.StatusActive},
		{VenueID: models.VenuePolymarket, VenueMarketID: "p2",
			Title: "Will BTC reach $100k by 2026?", Status: models.StatusActive},
		{VenueID: models.VenueKalshi, VenueMarketID: "k1",
			Title: "Will BTC reach $100k by 2026?", Status: models.StatusActive},
	}

	results := FindSignatureMatches(markets)
	for _, r := range results {
		if r.MarketA.VenueID == r.MarketB.VenueID {
			t.Error("signature match should only produce cross-venue pairs")
		}
	}
}

func TestFindEquivalentPairsFromIndex(t *testing.T) {
	cfg := &config.Config{
		MatchThreshold:         0.45,
		ProbableMatchThreshold: 0.35,
		MaxDateDeltaDays:       365,
	}
	m := New(cfg)

	now := time.Now()
	markets := []*models.CanonicalMarket{
		{VenueID: models.VenuePolymarket, VenueMarketID: "p1",
			Title: "Will Bitcoin hit $100,000 in 2026?", Status: models.StatusActive,
			YesPrice: 0.65, ResolutionDate: &now, Category: "crypto"},
		{VenueID: models.VenueKalshi, VenueMarketID: "k1",
			Title: "Will BTC reach $100k by end of 2026?", Status: models.StatusActive,
			YesPrice: 0.62, ResolutionDate: &now, Category: "crypto"},
		{VenueID: models.VenuePolymarket, VenueMarketID: "p2",
			Title: "Will Trump win 2028 election?", Status: models.StatusActive,
			YesPrice: 0.45, Category: "politics"},
	}

	ctx := context.Background()
	results := m.FindEquivalentPairsFromIndex(ctx, markets)
	// Just verify it runs without panicking and returns valid results
	for _, r := range results {
		if r.MarketA.VenueID == r.MarketB.VenueID {
			t.Error("should only produce cross-venue pairs")
		}
		if r.CompositeScore < 0 || r.CompositeScore > 1.5 {
			t.Errorf("composite score out of reasonable range: %.3f", r.CompositeScore)
		}
	}
}

func TestFindEquivalentPairsFromClustersNoPanic(t *testing.T) {
	cfg := &config.Config{
		MatchThreshold:         0.45,
		ProbableMatchThreshold: 0.35,
		MaxDateDeltaDays:       365,
	}
	m := New(cfg)

	markets := []*models.CanonicalMarket{
		{VenueID: models.VenuePolymarket, VenueMarketID: "p1",
			Title: "Will Bitcoin hit $100,000 in 2026?", Status: models.StatusActive,
			Category: "crypto"},
		{VenueID: models.VenueKalshi, VenueMarketID: "k1",
			Title: "Will BTC reach $100k by 2026?", Status: models.StatusActive,
			Category: "crypto"},
	}

	ctx := context.Background()
	results, clusterResults := m.FindEquivalentPairsFromClusters(ctx, markets)

	// Verify results only contain cross-venue pairs
	for _, r := range results {
		if r.MarketA.VenueID == r.MarketB.VenueID {
			t.Errorf("expected cross-venue pairs only, got same venue %s for %s vs %s",
				r.MarketA.VenueID, r.MarketA.VenueMarketID, r.MarketB.VenueMarketID)
		}
		if r.CompositeScore < 0 || r.CompositeScore > 1.5 {
			t.Errorf("composite score %.3f out of expected range [0, 1.5]", r.CompositeScore)
		}
	}

	// Verify cluster results contain valid data
	for _, cr := range clusterResults {
		for _, p := range cr.Pairs {
			if p.MarketA.VenueID == p.MarketB.VenueID {
				t.Errorf("cluster pair should be cross-venue, got same venue %s", p.MarketA.VenueID)
			}
			if p.CompositeScore < 0 || p.CompositeScore > 1.5 {
				t.Errorf("cluster pair composite score %.3f out of expected range [0, 1.5]", p.CompositeScore)
			}
		}
	}
}

func TestCategoryLabel(t *testing.T) {
	tests := []struct {
		catA, catB string
		want       string
	}{
		{"crypto", "crypto", "crypto"},
		{"crypto", "politics", "crypto/politics"},
		{"", "", "other"},
		{"crypto", "", "crypto/other"},
	}
	for _, tt := range tests {
		t.Run(tt.catA+"/"+tt.catB, func(t *testing.T) {
			a := &models.CanonicalMarket{Category: tt.catA}
			b := &models.CanonicalMarket{Category: tt.catB}
			got := categoryLabel(a, b)
			if got != tt.want {
				t.Errorf("categoryLabel = %q, want %q", got, tt.want)
			}
		})
	}
}
