package normalizer

import (
	"context"
	"testing"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/venues"
)

func TestNormalizePolymarket(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	payload := []byte(`{
		"id":"pm-001",
		"question":"Will the Federal Reserve cut rates?",
		"description":"Market resolves to yes if rates are cut.",
		"endDateIso":"2026-06-30T23:59:59Z",
		"outcomePrices":"[\"0.42\",\"0.58\"]",
		"volume":"120000",
		"liquidityNum":35000,
		"category":"economics",
		"tags":[{"label":"rates"},{"label":"US"}]
	}`)

	raw := []*venues.RawMarket{
		{
			VenueID:    models.VenuePolymarket,
			Payload:    payload,
			VenueMarketID: "pm-001",
		},
	}
	canonical, err := n.Normalize(context.Background(), raw)
	if err != nil {
		t.Fatalf("normalize returned error: %v", err)
	}
	if len(canonical) != 1 {
		t.Fatalf("expected 1 canonical market, got %d", len(canonical))
	}

	m := canonical[0]
	if m.VenueID != models.VenuePolymarket {
		t.Fatalf("unexpected venue: %s", m.VenueID)
	}
	if m.YesPrice != 0.42 || m.NoPrice != 0.58 {
		t.Fatalf("expected parsed prices 0.42/0.58, got %f/%f", m.YesPrice, m.NoPrice)
	}
	if m.ResolutionDate == nil || m.ResolutionDate.Year() != 2026 {
		t.Fatalf("expected parsed resolution date")
	}
}

func TestNormalizeKalshi(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	payload := []byte(`{
		"ticker":"KX-RATES-26",
		"title":"Will rates rise in Q3?",
		"subtitle":"",
		"category":"economics",
		"status":"active",
		"close_time":"2026-09-30T23:59:59Z",
		"yes_bid":55,
		"yes_ask":57,
		"no_bid":43,
		"no_ask":45,
		"volume":12000,
		"volume_24h":4000,
		"open_interest":2000,
		"liquidity":18000,
		"rules_primary":"Fallback to rules text."
	}`)

	raw := []*venues.RawMarket{
		{
			VenueID:    models.VenueKalshi,
			Payload:    payload,
			VenueMarketID: "KX-RATES-26",
		},
	}
	canonical, err := n.Normalize(context.Background(), raw)
	if err != nil {
		t.Fatalf("normalize returned error: %v", err)
	}
	if len(canonical) != 1 {
		t.Fatalf("expected 1 canonical market, got %d", len(canonical))
	}

	m := canonical[0]
	if m.Description != "Fallback to rules text." {
		t.Fatalf("expected rules fallback description, got %q", m.Description)
	}
	if m.YesPrice < 0.55 || m.YesPrice > 0.56 {
		t.Fatalf("unexpected yes price: %f", m.YesPrice)
	}
	if m.Spread != 0.02 {
		t.Fatalf("unexpected spread: %f", m.Spread)
	}
}

func TestNormalizeSkipsInvalidPayload(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	raw := []*venues.RawMarket{
		{
			VenueID:    models.VenuePolymarket,
			VenueMarketID: "pm-good",
			Payload:     []byte(`{"id":"pm-good","question":"valid"}`),
		},
		{
			VenueID:    models.VenuePolymarket,
			VenueMarketID: "pm-bad",
			Payload:     []byte(`{"invalid-json":`),
		},
	}

	canonical, err := n.Normalize(context.Background(), raw)
	if err != nil {
		t.Fatalf("normalize returned error: %v", err)
	}
	if len(canonical) != 1 {
		t.Fatalf("expected one valid market after skip, got %d", len(canonical))
	}
}
