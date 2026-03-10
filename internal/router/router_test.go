package router

import (
	"testing"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
)

func TestRouteSelectsBestVenueForYes(t *testing.T) {
	cfg := &config.Config{
		PriceWeight:     0.6,
		LiquidityWeight: 0.3,
		SpreadWeight:    0.1,
	}
	r := New(cfg)

	pair := &matcher.MatchResult{
		MarketA: &models.CanonicalMarket{
			VenueID:       models.VenuePolymarket,
			Title:         "Fed cut by Sep?",
			YesPrice:       0.45,
			Liquidity:      1000,
			Spread:         0,
		},
		MarketB: &models.CanonicalMarket{
			VenueID:       models.VenueKalshi,
			Title:         "Fed cut by Sep?",
			YesPrice:       0.20,
			Liquidity:      100,
			Spread:         0,
		},
	}

	decision := r.Route(&Order{
		EventTitle: "Fed cut by Sep?",
		Side:       SideYes,
		SizeUSD:    1000,
	}, pair)
	if decision.SelectedVenue.VenueID != models.VenuePolymarket {
		t.Fatalf("expected venue %s, got %s", models.VenuePolymarket, decision.SelectedVenue.VenueID)
	}
}

func TestRouteSelectsBestVenueForNo(t *testing.T) {
	cfg := &config.Config{
		PriceWeight:     0.6,
		LiquidityWeight: 0.3,
		SpreadWeight:    0.1,
	}
	r := New(cfg)

	pair := &matcher.MatchResult{
		MarketA: &models.CanonicalMarket{
			VenueID:  models.VenuePolymarket,
			YesPrice: 0.20,
			Liquidity: 1000,
			Spread:    0,
		},
		MarketB: &models.CanonicalMarket{
			VenueID:  models.VenueKalshi,
			YesPrice: 0.70,
			Liquidity: 1000,
			Spread:    0,
		},
	}

	decision := r.Route(&Order{
		EventTitle: "Fed no-cut by Sep?",
		Side:       SideNo,
		SizeUSD:    1000,
	}, pair)
	if decision.SelectedVenue.VenueID != models.VenueKalshi {
		t.Fatalf("expected venue %s, got %s", models.VenueKalshi, decision.SelectedVenue.VenueID)
	}
}
