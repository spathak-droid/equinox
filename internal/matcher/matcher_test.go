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
	m := New(cfg, nil)

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
	m := New(cfg, nil)

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
