package normalizer

import (
	"context"
	"encoding/json"
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

	raw := []*venues.RawMarket{{VenueID: models.VenuePolymarket, Payload: payload, VenueMarketID: "pm-001"}}
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
		t.Fatalf("expected prices 0.42/0.58, got %f/%f", m.YesPrice, m.NoPrice)
	}
	if m.ResolutionDate == nil || m.ResolutionDate.Year() != 2026 {
		t.Fatalf("expected parsed resolution date")
	}
	if m.Category != "economics" {
		t.Errorf("expected category economics, got %s", m.Category)
	}
	if m.Liquidity != 35000 {
		t.Errorf("expected liquidity 35000, got %f", m.Liquidity)
	}
}

func TestNormalizePolymarketPublicSearch(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	// public-search payloads have no "id" field, use slug instead
	payload := []byte(`{
		"slug":"will-btc-100k",
		"question":"Will Bitcoin reach $100k?",
		"endDateIso":"2026-12-31T23:59:59Z",
		"bestBid":0.62,
		"bestAsk":0.65,
		"spread":0.03,
		"liquidityNum":50000
	}`)

	raw := []*venues.RawMarket{{VenueID: models.VenuePolymarket, Payload: payload, VenueMarketID: "will-btc-100k"}}
	canonical, err := n.Normalize(context.Background(), raw)
	if err != nil {
		t.Fatalf("normalize error: %v", err)
	}
	if len(canonical) != 1 {
		t.Fatalf("expected 1, got %d", len(canonical))
	}

	m := canonical[0]
	if m.VenueMarketID != "will-btc-100k" {
		t.Errorf("expected slug as market ID, got %s", m.VenueMarketID)
	}
	if m.Spread != 0.03 {
		t.Errorf("expected spread 0.03, got %f", m.Spread)
	}
	// bestBid/bestAsk midpoint = (0.62+0.65)/2 = 0.635
	if m.YesPrice < 0.63 || m.YesPrice > 0.64 {
		t.Errorf("expected yes price ~0.635 from bid/ask midpoint, got %f", m.YesPrice)
	}
}

func TestNormalizePolymarketGroupItemTitle(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	payload := []byte(`{
		"id":"pm-group",
		"question":"Will Spain win the World Cup?",
		"groupItemTitle":"Spain"
	}`)

	raw := []*venues.RawMarket{{VenueID: models.VenuePolymarket, Payload: payload, VenueMarketID: "pm-group"}}
	canonical, _ := n.Normalize(context.Background(), raw)
	if len(canonical) != 1 {
		t.Fatalf("expected 1, got %d", len(canonical))
	}
	if canonical[0].Subtitle != "Spain" {
		t.Errorf("expected subtitle 'Spain', got %q", canonical[0].Subtitle)
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

	raw := []*venues.RawMarket{{VenueID: models.VenueKalshi, Payload: payload, VenueMarketID: "KX-RATES-26"}}
	canonical, err := n.Normalize(context.Background(), raw)
	if err != nil {
		t.Fatalf("normalize error: %v", err)
	}
	if len(canonical) != 1 {
		t.Fatalf("expected 1 canonical market, got %d", len(canonical))
	}

	m := canonical[0]
	if m.Description != "Fallback to rules text." {
		t.Fatalf("expected rules fallback description, got %q", m.Description)
	}
	// yesMid = (55+57)/2/100 = 0.56
	if m.YesPrice < 0.55 || m.YesPrice > 0.57 {
		t.Fatalf("unexpected yes price: %f", m.YesPrice)
	}
	// spread = (57-55)/100 = 0.02
	if m.Spread != 0.02 {
		t.Fatalf("unexpected spread: %f", m.Spread)
	}
	if m.Volume24h != 4000 {
		t.Errorf("expected volume24h 4000, got %f", m.Volume24h)
	}
	if m.OpenInterest != 2000 {
		t.Errorf("expected open interest 2000, got %f", m.OpenInterest)
	}
}

func TestNormalizeKalshiWithEventTitle(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	payload := []byte(`{
		"ticker":"KX-FIFA-SPAIN",
		"event_title":"2026 FIFA World Cup Winner",
		"title":"",
		"subtitle":"Spain",
		"status":"active",
		"yes_bid":20,
		"yes_ask":25,
		"no_bid":75,
		"no_ask":80,
		"volume":5000,
		"volume_24h":1000
	}`)

	raw := []*venues.RawMarket{{VenueID: models.VenueKalshi, Payload: payload, VenueMarketID: "KX-FIFA-SPAIN"}}
	canonical, _ := n.Normalize(context.Background(), raw)
	if len(canonical) != 1 {
		t.Fatalf("expected 1, got %d", len(canonical))
	}

	m := canonical[0]
	// Title should be "event_title — subtitle"
	if m.Title != "2026 FIFA World Cup Winner — Spain" {
		t.Errorf("expected combined title, got %q", m.Title)
	}
	if m.VenueEventTitle != "2026 FIFA World Cup Winner" {
		t.Errorf("expected venue event title, got %q", m.VenueEventTitle)
	}
}

func TestNormalizeKalshiEstimateLiquidity(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	// Kalshi API returns liquidity=0 in practice; we estimate from volume × (1 - spread)
	payload := []byte(`{
		"ticker":"KX-TEST",
		"title":"Test",
		"status":"active",
		"yes_bid":40,
		"yes_ask":60,
		"no_bid":40,
		"no_ask":60,
		"volume":10000,
		"volume_24h":5000
	}`)

	raw := []*venues.RawMarket{{VenueID: models.VenueKalshi, Payload: payload, VenueMarketID: "KX-TEST"}}
	canonical, _ := n.Normalize(context.Background(), raw)
	if len(canonical) != 1 {
		t.Fatalf("expected 1, got %d", len(canonical))
	}

	m := canonical[0]
	// spread = (60-40)/100 = 0.20, volume=10000, est. liq = 10000 * (1-0.20) = 8000
	if m.Liquidity < 7900 || m.Liquidity > 8100 {
		t.Errorf("expected estimated liquidity ~8000, got %f", m.Liquidity)
	}
}

func TestNormalizeSkipsInvalidPayload(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	raw := []*venues.RawMarket{
		{VenueID: models.VenuePolymarket, VenueMarketID: "pm-good",
			Payload: []byte(`{"id":"pm-good","question":"valid"}`)},
		{VenueID: models.VenuePolymarket, VenueMarketID: "pm-bad",
			Payload: []byte(`{"invalid-json":`)},
	}

	canonical, err := n.Normalize(context.Background(), raw)
	if err != nil {
		t.Fatalf("normalize returned error: %v", err)
	}
	if len(canonical) != 1 {
		t.Fatalf("expected one valid market after skip, got %d", len(canonical))
	}
}

func TestNormalizeUnknownVenue(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	raw := []*venues.RawMarket{
		{VenueID: "unknown_venue", VenueMarketID: "m1", Payload: []byte(`{}`)},
	}

	_, err := n.Normalize(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for unknown venue")
	}
}

func TestNormalizePreservesRawPayload(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	payload := []byte(`{"id":"pm-raw","question":"Test raw payload","custom_field":"preserved"}`)
	raw := []*venues.RawMarket{{VenueID: models.VenuePolymarket, VenueMarketID: "pm-raw", Payload: payload}}
	canonical, _ := n.Normalize(context.Background(), raw)
	if len(canonical) != 1 {
		t.Fatalf("expected 1, got %d", len(canonical))
	}

	// RawPayload should contain the original JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal(canonical[0].RawPayload, &parsed); err != nil {
		t.Fatalf("failed to parse raw payload: %v", err)
	}
	if parsed["custom_field"] != "preserved" {
		t.Error("raw payload should preserve original fields")
	}
}

func TestNormalizeDateParsingFormats(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	// Test RFC3339 format
	tests := []struct {
		name    string
		dateStr string
		wantNil bool
	}{
		{"RFC3339", `"2026-06-30T23:59:59Z"`, false},
		{"ISO8601 no timezone", `"2026-06-30T23:59:59Z"`, false},
		{"date only", `"2026-06-30"`, false},
		{"empty", `""`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"id":"pm-date","question":"Test","endDateIso":` + tt.dateStr + `}`)
			raw := []*venues.RawMarket{{VenueID: models.VenuePolymarket, VenueMarketID: "pm-date", Payload: payload}}
			canonical, _ := n.Normalize(context.Background(), raw)
			if len(canonical) != 1 {
				t.Fatalf("expected 1, got %d", len(canonical))
			}
			if tt.wantNil && canonical[0].ResolutionDate != nil {
				t.Error("expected nil resolution date")
			}
			if !tt.wantNil && canonical[0].ResolutionDate == nil {
				t.Error("expected non-nil resolution date")
			}
		})
	}
}

func TestKalshiCanonicalTitle(t *testing.T) {
	tests := []struct {
		event, sub, rules string
		want              string
	}{
		{"World Cup Winner", "Spain", "", "World Cup Winner — Spain"},
		{"World Cup Winner", "", "", "World Cup Winner"},
		{"", "Spain", "", "Spain"},
		{"", "", "", ""},
		// Party-label subtitles: extract candidate from rules_primary
		{
			"2028 U.S. Presidential Election winner?",
			":: Democratic",
			"If Mark Kelly is the next person inaugurated as President for the term beginning in 2029, then the market resolves to Yes.",
			"2028 U.S. Presidential Election winner? — Mark Kelly",
		},
		// Non-party :: subtitle with no rules → strip :: prefix, keep name
		{
			"Best Picture Winner",
			":: Drama",
			"",
			"Best Picture Winner — Drama",
		},
		// Named subtitle (no ::) stays as-is
		{
			"2028 U.S. Presidential Election winner?",
			"Jamie Dimon",
			"If Jamie Dimon is the next person inaugurated as President, then Yes.",
			"2028 U.S. Presidential Election winner? — Jamie Dimon",
		},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := kalshiCanonicalTitle(tt.event, tt.sub, tt.rules)
			if got != tt.want {
				t.Errorf("kalshiCanonicalTitle(%q, %q, %q) = %q, want %q", tt.event, tt.sub, tt.rules, got, tt.want)
			}
		})
	}
}

func TestEstimateKalshiLiquidity(t *testing.T) {
	tests := []struct {
		name   string
		raw    kalshiRaw
		expect float64
	}{
		{
			"normal",
			kalshiRaw{Volume: 10000, Volume24h: 5000, YesBid: 45, YesAsk: 55},
			10000 * 0.9, // spread = 10/100 = 0.10, liq = 10000 * 0.90
		},
		{
			"24h volume higher",
			kalshiRaw{Volume: 1000, Volume24h: 5000, YesBid: 48, YesAsk: 52},
			5000 * 0.96, // spread = 4/100 = 0.04, liq = 5000 * 0.96
		},
		{
			"zero spread",
			kalshiRaw{Volume: 1000, YesBid: 50, YesAsk: 50},
			1000, // spread=0, liq = 1000 * 1.0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateKalshiLiquidity(tt.raw)
			diff := got - tt.expect
			if diff < -1 || diff > 1 {
				t.Errorf("estimateKalshiLiquidity = %.2f, want ~%.2f", got, tt.expect)
			}
		})
	}
}

func TestNormalizeCategoryInNormalizer(t *testing.T) {
	cfg := &config.Config{}
	n := New(cfg)

	payload := []byte(`{"id":"pm-cat","question":"Test","category":"Finance"}`)
	raw := []*venues.RawMarket{{VenueID: models.VenuePolymarket, VenueMarketID: "pm-cat", Payload: payload}}
	canonical, _ := n.Normalize(context.Background(), raw)
	if len(canonical) != 1 {
		t.Fatalf("expected 1, got %d", len(canonical))
	}
	if canonical[0].Category != "economics" {
		t.Errorf("expected 'finance' normalized to 'economics', got %q", canonical[0].Category)
	}
}
