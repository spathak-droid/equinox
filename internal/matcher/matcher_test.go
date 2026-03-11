package matcher

import (
	"context"
	"testing"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
)

func TestFindEquivalentPairsCrossVenueOnlyAndThreshold(t *testing.T) {
	cfg := &config.Config{
		MatchThreshold:         0.45,
		ProbableMatchThreshold: 0.35,
		MaxDateDeltaDays:       365,
	}
	m := New(cfg)

	now := time.Now().UTC().Truncate(time.Second)
	a := &models.CanonicalMarket{
		VenueID:       models.VenuePolymarket,
		Title:         "Will inflation be above 3% in June?",
		Status:        models.StatusActive,
		ResolutionDate: &now,
	}
	b := &models.CanonicalMarket{
		VenueID:       models.VenueKalshi,
		Title:         "Will inflation be above 3% in June?",
		Status:        models.StatusActive,
		ResolutionDate: &now,
	}
	c := &models.CanonicalMarket{
		VenueID: models.VenueKalshi,
		Title:   "Unrelated market from the same venue",
		Status:  models.StatusActive,
	}
	d := &models.CanonicalMarket{
		VenueID: models.VenuePolymarket,
		Title:   "Will inflation be above 3% in June?",
		Status:  models.StatusActive,
	}

	pairs := m.FindEquivalentPairs(context.Background(), []*models.CanonicalMarket{a, b, c, d})
	// a↔b and d↔b are both cross-venue matches with identical titles → 2 pairs
	if len(pairs) != 2 {
		t.Fatalf("expected 2 matched pairs (a↔b and d↔b are both cross-venue), got %d", len(pairs))
	}
}

func TestHardFiltersDateGate(t *testing.T) {
	cfg := &config.Config{
		MatchThreshold:         0.45,
		ProbableMatchThreshold: 0.35,
		MaxDateDeltaDays:       10,
	}
	m := New(cfg)

	soon := time.Now().UTC()
	later := soon.Add(365 * 24 * time.Hour)

	a := &models.CanonicalMarket{
		VenueID:       models.VenuePolymarket,
		Title:         "Policy rate change in 2026?",
		Status:        models.StatusActive,
		ResolutionDate: &soon,
	}
	b := &models.CanonicalMarket{
		VenueID:       models.VenueKalshi,
		Title:         "Policy rate change in 2026?",
		Status:        models.StatusActive,
		ResolutionDate: &later,
	}

	pairs := m.FindEquivalentPairs(context.Background(), []*models.CanonicalMarket{a, b})
	if len(pairs) != 0 {
		t.Fatalf("expected no matched pairs due to date gate, got %d", len(pairs))
	}
}

func TestFuzzyTitleScore(t *testing.T) {
	s := fuzzyTitleScore("Will inflation fall below 2%?", "Will inflation fall below 2%?")
	if s != 1.0 {
		t.Fatalf("expected exact match score 1.0, got %f", s)
	}

	s2 := fuzzyTitleScore("Will inflation fall below 2%?", "Will the moon be made of cheese?")
	if s2 >= 0.35 {
		t.Fatalf("expected weak match for unrelated titles, got %f", s2)
	}
}

// --- Golden test fixtures ---

func makeMarket(venue models.VenueID, id, title string, yesPrice float64, resDate *time.Time) *models.CanonicalMarket {
	return &models.CanonicalMarket{
		ID:             id,
		VenueID:        venue,
		VenueMarketID:  id,
		Title:          title,
		YesPrice:       yesPrice,
		NoPrice:        1.0 - yesPrice,
		Status:         models.StatusActive,
		Category:       "other",
		ResolutionDate: resDate,
	}
}

func datePtr(y, m, d int) *time.Time {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return &t
}

func testConfig() *config.Config {
	return &config.Config{
		MatchThreshold:         0.45,
		ProbableMatchThreshold: 0.35,
		MaxDateDeltaDays:       365,
		PriceWeight:            0.60,
		LiquidityWeight:        0.30,
		SpreadWeight:           0.10,
	}
}

// TestGoldenPairs tests known-equivalent and known-different market pairs
// through the full comparison pipeline to catch regressions.
func TestGoldenPairs(t *testing.T) {
	cfg := testConfig()
	m := New(cfg)

	// Golden matches: these SHOULD produce match or probable
	matches := []struct {
		name   string
		a, b   *models.CanonicalMarket
		minScr float64
	}{
		{
			name:   "BTC $100k same event different venues",
			a:      makeMarket(models.VenuePolymarket, "poly-btc100k", "Will Bitcoin hit $100,000 in 2026?", 0.65, datePtr(2026, 12, 31)),
			b:      makeMarket(models.VenueKalshi, "kalshi-btc100k", "Will BTC reach $100k by end of 2026?", 0.62, datePtr(2026, 12, 31)),
			minScr: 0.35,
		},
		{
			name:   "Trump election same event",
			a:      makeMarket(models.VenuePolymarket, "poly-trump", "Will Trump win the 2028 Presidential Election?", 0.45, datePtr(2028, 11, 5)),
			b:      makeMarket(models.VenueKalshi, "kalshi-trump", "Will Donald Trump win the 2028 election?", 0.48, datePtr(2028, 11, 5)),
			minScr: 0.35,
		},
		{
			name:   "Fed rate cut same event",
			a:      makeMarket(models.VenuePolymarket, "poly-fed", "Will the Fed cut interest rates in March 2026?", 0.30, datePtr(2026, 3, 31)),
			b:      makeMarket(models.VenueKalshi, "kalshi-fed", "Federal Reserve rate cut in March 2026?", 0.28, datePtr(2026, 3, 31)),
			minScr: 0.35,
		},
	}

	for _, tt := range matches {
		t.Run("match/"+tt.name, func(t *testing.T) {
			result := m.compare(tt.a, tt.b)
			if result.CompositeScore < tt.minScr {
				t.Errorf("expected match but composite=%.3f < %.2f\n  explanation: %s",
					result.CompositeScore, tt.minScr, result.Explanation)
			}
			if result.Confidence == ConfidenceNoMatch {
				t.Errorf("expected match/probable but got NO_MATCH\n  explanation: %s", result.Explanation)
			}
		})
	}

	// Golden non-matches: these should NOT match
	nonMatches := []struct {
		name string
		a, b *models.CanonicalMarket
	}{
		{
			name: "BTC different thresholds $100k vs $50k",
			a:    makeMarket(models.VenuePolymarket, "poly-btc100k", "Will Bitcoin hit $100,000 in 2026?", 0.65, datePtr(2026, 12, 31)),
			b:    makeMarket(models.VenueKalshi, "kalshi-btc50k", "Will Bitcoin hit $50,000 in 2026?", 0.90, datePtr(2026, 12, 31)),
		},
		{
			name: "Different candidates same election",
			a:    makeMarket(models.VenuePolymarket, "poly-trump", "Will Trump win the 2028 Presidential Election?", 0.45, datePtr(2028, 11, 5)),
			b:    makeMarket(models.VenueKalshi, "kalshi-harris", "Will Harris win the 2028 Presidential Election?", 0.35, datePtr(2028, 11, 5)),
		},
		{
			name: "Same entity completely different question",
			a:    makeMarket(models.VenuePolymarket, "poly-trump-elect", "Will Trump win the 2028 election?", 0.45, datePtr(2028, 11, 5)),
			b:    makeMarket(models.VenueKalshi, "kalshi-trump-impeach", "Will Trump be impeached before 2028?", 0.10, datePtr(2028, 1, 1)),
		},
	}

	for _, tt := range nonMatches {
		t.Run("no-match/"+tt.name, func(t *testing.T) {
			result := m.compare(tt.a, tt.b)
			if result.Confidence == ConfidenceMatch {
				t.Errorf("expected NO_MATCH but got MATCH (composite=%.3f)\n  explanation: %s",
					result.CompositeScore, result.Explanation)
			}
		})
	}
}

// TestSignaturePrePass verifies that FindSignatureMatches correctly identifies
// cross-venue markets with identical semantic signatures.
func TestSignaturePrePass(t *testing.T) {
	markets := []*models.CanonicalMarket{
		makeMarket(models.VenuePolymarket, "poly-1", "Will Bitcoin hit $100,000 in 2026?", 0.65, datePtr(2026, 12, 31)),
		makeMarket(models.VenueKalshi, "kalshi-1", "Will BTC reach $100k by 2026?", 0.62, datePtr(2026, 12, 31)),
		makeMarket(models.VenuePolymarket, "poly-2", "Will Bitcoin hit $50,000 in 2026?", 0.90, datePtr(2026, 12, 31)),
		makeMarket(models.VenueKalshi, "kalshi-2", "Will Trump win the 2028 election?", 0.45, datePtr(2028, 11, 5)),
	}

	results := FindSignatureMatches(markets)

	for _, r := range results {
		if r.MarketA.VenueID == r.MarketB.VenueID {
			t.Error("signature match should only be cross-venue")
		}
		if !r.SignatureMatch {
			t.Error("signature match results should have SignatureMatch=true")
		}
		if r.CompositeScore != 1.0 {
			t.Errorf("signature match should have composite=1.0, got %.2f", r.CompositeScore)
		}
	}
}

// TestSemanticGateRejectsEarly verifies that the semantic gate in Stage 1
// rejects incompatible pairs before reaching the scoring pipeline.
func TestSemanticGateRejectsEarly(t *testing.T) {
	cfg := testConfig()
	m := New(cfg)

	// Different threshold: should be rejected by semantic gate
	a := makeMarket(models.VenuePolymarket, "poly-btc100k", "Will Bitcoin hit $100,000 by June 2026?", 0.65, datePtr(2026, 6, 30))
	b := makeMarket(models.VenueKalshi, "kalshi-btc150k", "Will Bitcoin hit $150,000 by June 2026?", 0.40, datePtr(2026, 6, 30))

	result := m.compare(a, b)

	if result.Confidence == ConfidenceMatch {
		t.Errorf("semantic gate should reject different thresholds, got MATCH (composite=%.3f)\n  %s",
			result.CompositeScore, result.Explanation)
	}
}

// TestFullPipelineIntegration runs the cluster-based pipeline end-to-end
// with a small dataset to verify the full integration works.
func TestFullPipelineIntegration(t *testing.T) {
	cfg := testConfig()
	m := New(cfg)

	markets := []*models.CanonicalMarket{
		// Pair 1: BTC $100k — should match
		makeMarket(models.VenuePolymarket, "poly-btc100k", "Will Bitcoin hit $100,000 in 2026?", 0.65, datePtr(2026, 12, 31)),
		makeMarket(models.VenueKalshi, "kalshi-btc100k", "Will BTC reach $100k by end of 2026?", 0.62, datePtr(2026, 12, 31)),
		// Pair 2: different candidates — should NOT match each other
		makeMarket(models.VenuePolymarket, "poly-trump", "Will Trump win the 2028 election?", 0.45, datePtr(2028, 11, 5)),
		makeMarket(models.VenueKalshi, "kalshi-harris", "Will Harris win the 2028 election?", 0.35, datePtr(2028, 11, 5)),
		// Noise: completely unrelated market
		makeMarket(models.VenuePolymarket, "poly-weather", "Will 2026 be the hottest year on record?", 0.70, datePtr(2026, 12, 31)),
	}

	ctx := context.Background()
	pairs, _ := m.FindEquivalentPairsFromClusters(ctx, markets)

	// Verify no false positives: Trump should not match Harris
	for _, p := range pairs {
		aID := p.MarketA.VenueMarketID
		bID := p.MarketB.VenueMarketID
		if (aID == "poly-trump" && bID == "kalshi-harris") ||
			(aID == "kalshi-harris" && bID == "poly-trump") {
			t.Errorf("false positive: Trump matched Harris (composite=%.3f)\n  %s",
				p.CompositeScore, p.Explanation)
		}
	}
}
