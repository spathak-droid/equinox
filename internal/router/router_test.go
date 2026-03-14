package router

import (
	"math"
	"strings"
	"testing"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
)

func testCfg() *config.Config {
	return &config.Config{
		PriceWeight:     0.6,
		LiquidityWeight: 0.3,
		SpreadWeight:    0.1,
	}
}

func TestRouteSelectsBestVenueForYes(t *testing.T) {
	r := New(testCfg())

	pair := &matcher.MatchResult{
		MarketA: &models.CanonicalMarket{
			VenueID: models.VenuePolymarket, Title: "Fed cut by Sep?",
			YesPrice: 0.45, Liquidity: 1000, Spread: 0,
		},
		MarketB: &models.CanonicalMarket{
			VenueID: models.VenueKalshi, Title: "Fed cut by Sep?",
			YesPrice: 0.20, Liquidity: 100, Spread: 0,
		},
	}

	decision := r.Route(&Order{EventTitle: "Fed cut by Sep?", Side: SideYes, SizeUSD: 1000}, pair)
	if decision.SelectedVenue.VenueID != models.VenuePolymarket {
		t.Fatalf("expected %s, got %s", models.VenuePolymarket, decision.SelectedVenue.VenueID)
	}
}

func TestRouteSelectsBestVenueForNo(t *testing.T) {
	r := New(testCfg())

	pair := &matcher.MatchResult{
		MarketA: &models.CanonicalMarket{
			VenueID: models.VenuePolymarket, YesPrice: 0.20, Liquidity: 1000, Spread: 0,
		},
		MarketB: &models.CanonicalMarket{
			VenueID: models.VenueKalshi, YesPrice: 0.70, Liquidity: 1000, Spread: 0,
		},
	}

	decision := r.Route(&Order{EventTitle: "Fed no-cut", Side: SideNo, SizeUSD: 1000}, pair)
	if decision.SelectedVenue.VenueID != models.VenueKalshi {
		t.Fatalf("expected %s, got %s", models.VenueKalshi, decision.SelectedVenue.VenueID)
	}
}

func TestScoreVenuePriceScoreYes(t *testing.T) {
	r := New(testCfg())

	m := &models.CanonicalMarket{VenueID: models.VenuePolymarket, YesPrice: 0.40, Liquidity: 1000}
	order := &Order{Side: SideYes, SizeUSD: 1000}
	s := r.scoreVenue(m, order)

	// For YES: priceScore = 1 - YesPrice = 1 - 0.40 = 0.60
	if math.Abs(s.PriceScore-0.60) > 0.001 {
		t.Errorf("YES price score = %.4f, want 0.60", s.PriceScore)
	}
}

func TestScoreVenuePriceScoreNo(t *testing.T) {
	r := New(testCfg())

	m := &models.CanonicalMarket{VenueID: models.VenueKalshi, YesPrice: 0.70, Liquidity: 1000}
	order := &Order{Side: SideNo, SizeUSD: 1000}
	s := r.scoreVenue(m, order)

	// For NO: priceScore = YesPrice = 0.70
	if math.Abs(s.PriceScore-0.70) > 0.001 {
		t.Errorf("NO price score = %.4f, want 0.70", s.PriceScore)
	}
}

func TestScoreVenueLiquidityScore(t *testing.T) {
	r := New(testCfg())

	tests := []struct {
		name      string
		liquidity float64
		orderSize float64
		minScore  float64
		maxScore  float64
	}{
		{"high liquidity", 10000, 1000, 0.99, 1.0},     // tanh(10) ≈ 1.0
		{"equal to order", 1000, 1000, 0.7, 0.8},       // tanh(1) ≈ 0.76
		{"low liquidity", 100, 1000, 0.05, 0.15},        // tanh(0.1) ≈ 0.10
		{"zero liquidity", 0, 1000, 0.0, 0.001},         // tanh(0) = 0
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &models.CanonicalMarket{VenueID: models.VenuePolymarket, YesPrice: 0.5, Liquidity: tt.liquidity}
			order := &Order{Side: SideYes, SizeUSD: tt.orderSize}
			s := r.scoreVenue(m, order)
			if s.LiquidityScore < tt.minScore || s.LiquidityScore > tt.maxScore {
				t.Errorf("liquidity score = %.4f, want [%.2f, %.2f]", s.LiquidityScore, tt.minScore, tt.maxScore)
			}
		})
	}
}

func TestScoreVenueSpreadScore(t *testing.T) {
	r := New(testCfg())

	tests := []struct {
		name   string
		spread float64
		want   float64
	}{
		{"no spread data", 0, 0.5},
		{"tight spread", 0.01, 0.95},
		{"moderate spread", 0.10, 0.50},
		{"very wide spread", 0.20, 0.0},
		{"extreme spread capped", 0.50, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &models.CanonicalMarket{VenueID: models.VenuePolymarket, YesPrice: 0.5, Spread: tt.spread, Liquidity: 1000}
			order := &Order{Side: SideYes, SizeUSD: 1000}
			s := r.scoreVenue(m, order)
			if math.Abs(s.SpreadScore-tt.want) > 0.01 {
				t.Errorf("spread score = %.4f, want %.4f", s.SpreadScore, tt.want)
			}
		})
	}
}

func TestScoreVenueZeroOrderSize(t *testing.T) {
	r := New(testCfg())

	m := &models.CanonicalMarket{VenueID: models.VenuePolymarket, YesPrice: 0.5, Liquidity: 1000}
	order := &Order{Side: SideYes, SizeUSD: 0}
	s := r.scoreVenue(m, order)

	// Should not panic, order size adjusted to 1.0
	if s.TotalScore <= 0 {
		t.Errorf("expected positive score even with zero order size, got %.4f", s.TotalScore)
	}
	if s.TotalScore > 1.0 {
		t.Errorf("expected score <= 1.0, got %.4f", s.TotalScore)
	}
	// Price score for YES with YesPrice=0.5 should be 0.5
	if math.Abs(s.PriceScore-0.5) > 0.001 {
		t.Errorf("price score = %.4f, want 0.5", s.PriceScore)
	}
	// Liquidity score with 1000 liquidity and adjusted order size of 1.0: tanh(1000) ~ 1.0
	if s.LiquidityScore < 0.99 {
		t.Errorf("liquidity score = %.4f, want ~1.0 (high liquidity vs adjusted order size)", s.LiquidityScore)
	}
}

func TestRouteExplanationContainsKey(t *testing.T) {
	r := New(testCfg())

	pair := &matcher.MatchResult{
		Confidence: matcher.ConfidenceMatch,
		CompositeScore: 0.85,
		MarketA: &models.CanonicalMarket{
			VenueID: models.VenuePolymarket, YesPrice: 0.40, Liquidity: 5000, Spread: 0.01,
		},
		MarketB: &models.CanonicalMarket{
			VenueID: models.VenueKalshi, YesPrice: 0.42, Liquidity: 3000, Spread: 0.03,
		},
	}

	decision := r.Route(&Order{EventTitle: "Test", Side: SideYes, SizeUSD: 1000}, pair)

	// Explanation should contain key sections
	for _, section := range []string{"ROUTING DECISION", "Venue Comparison", "Weights", "Estimated Execution"} {
		if !strings.Contains(decision.Explanation, section) {
			t.Errorf("explanation missing section %q", section)
		}
	}
}

func TestRouteLiquidityWarningInExplanation(t *testing.T) {
	r := New(testCfg())

	// Both venues have liquidity well below the $1000 order size,
	// so the selected venue must trigger the "NOT enough" warning.
	pair := &matcher.MatchResult{
		Confidence:     matcher.ConfidenceMatch,
		CompositeScore: 0.85,
		MarketA: &models.CanonicalMarket{
			VenueID: models.VenuePolymarket, YesPrice: 0.30, Liquidity: 200, Spread: 0,
		},
		MarketB: &models.CanonicalMarket{
			VenueID: models.VenueKalshi, YesPrice: 0.90, Liquidity: 50, Spread: 0,
		},
	}

	decision := r.Route(&Order{EventTitle: "Test", Side: SideYes, SizeUSD: 1000}, pair)

	// The selected venue (Polymarket, liquidity=200) has liquidity < order size (1000),
	// so the explanation must contain the "NOT enough" warning.
	if !strings.Contains(decision.Explanation, "NOT enough") {
		t.Errorf("expected 'NOT enough' liquidity warning in explanation when liquidity ($200) < order size ($1000).\nExplanation:\n%s", decision.Explanation)
	}
}

func TestRouteWeightsSumToOne(t *testing.T) {
	cfg := testCfg()
	sum := cfg.PriceWeight + cfg.LiquidityWeight + cfg.SpreadWeight
	if math.Abs(sum-1.0) > 0.001 {
		t.Errorf("routing weights sum to %.3f, should be 1.0", sum)
	}
}

func TestBuildReasonsOnlyOneVenue(t *testing.T) {
	reasons := buildReasons(&Order{Side: SideYes, SizeUSD: 1000}, nil, nil)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "Only one venue") {
		t.Errorf("expected single venue message, got %v", reasons)
	}
}

func TestBuildReasonsPriceAdvantage(t *testing.T) {
	winner := &VenueScore{
		Market: &models.CanonicalMarket{VenueID: models.VenuePolymarket, YesPrice: 0.30, Liquidity: 1000},
	}
	loser := &VenueScore{
		Market: &models.CanonicalMarket{VenueID: models.VenueKalshi, YesPrice: 0.50, Liquidity: 1000},
	}

	reasons := buildReasons(&Order{Side: SideYes, SizeUSD: 1000}, winner, loser)
	if len(reasons) == 0 {
		t.Fatal("expected at least one reason")
	}
	found := false
	for _, r := range reasons {
		if strings.Contains(r, "Better price") || strings.Contains(r, "cheaper") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected price advantage reason, got %v", reasons)
	}
}

func TestRouteDecisionHasAllScores(t *testing.T) {
	r := New(testCfg())

	pair := &matcher.MatchResult{
		Confidence: matcher.ConfidenceMatch,
		MarketA: &models.CanonicalMarket{
			VenueID: models.VenuePolymarket, YesPrice: 0.40, Liquidity: 1000,
		},
		MarketB: &models.CanonicalMarket{
			VenueID: models.VenueKalshi, YesPrice: 0.60, Liquidity: 1000,
		},
	}

	decision := r.Route(&Order{EventTitle: "Test", Side: SideYes, SizeUSD: 1000}, pair)

	if len(decision.AllScores) != 2 {
		t.Fatalf("expected 2 venue scores, got %d", len(decision.AllScores))
	}
	if decision.SelectedVenue == nil {
		t.Fatal("selected venue should not be nil")
	}
	if decision.FinalScore <= 0 {
		t.Errorf("final score should be positive, got %.4f", decision.FinalScore)
	}
}

func TestRouteSpreadTiebreaker(t *testing.T) {
	r := New(testCfg())

	// Same price and liquidity — spread should break the tie
	pair := &matcher.MatchResult{
		MarketA: &models.CanonicalMarket{
			VenueID: models.VenuePolymarket, YesPrice: 0.50, Liquidity: 1000, Spread: 0.02,
		},
		MarketB: &models.CanonicalMarket{
			VenueID: models.VenueKalshi, YesPrice: 0.50, Liquidity: 1000, Spread: 0.10,
		},
	}

	decision := r.Route(&Order{EventTitle: "Tiebreaker", Side: SideYes, SizeUSD: 1000}, pair)
	if decision.SelectedVenue.VenueID != models.VenuePolymarket {
		t.Errorf("expected Polymarket (tighter spread), got %s", decision.SelectedVenue.VenueID)
	}
}
