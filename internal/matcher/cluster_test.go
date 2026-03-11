package matcher

import (
	"testing"

	"github.com/equinox/internal/models"
)

func TestClusterByTopic(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueID: models.VenuePolymarket, VenueMarketID: "p1",
			Title: "Will Bitcoin hit $100k?", Category: "crypto"},
		{VenueID: models.VenueKalshi, VenueMarketID: "k1",
			Title: "Will Bitcoin reach $100k?", Category: "crypto"},
		{VenueID: models.VenuePolymarket, VenueMarketID: "p2",
			Title: "Will Trump win 2028?", Category: "politics"},
		{VenueID: models.VenueKalshi, VenueMarketID: "k2",
			Title: "Will Trump be elected?", Category: "politics"},
		{VenueID: models.VenuePolymarket, VenueMarketID: "p3",
			Title: "Will it rain tomorrow?", Category: "other"},
	}

	clusters := ClusterByTopic(markets)

	if len(clusters) == 0 {
		t.Fatal("expected at least one cluster")
	}

	// Verify cross-venue clusters are sorted first
	sawSingleVenue := false
	for _, c := range clusters {
		if !c.HasCrossVenue() {
			sawSingleVenue = true
		} else if sawSingleVenue {
			t.Error("cross-venue clusters should come before single-venue clusters")
		}
	}
}

func TestClusterByTopicNoDuplicateMarkets(t *testing.T) {
	markets := []*models.CanonicalMarket{
		{VenueID: models.VenuePolymarket, VenueMarketID: "p1",
			Title: "Bitcoin Ethereum DeFi", Category: "crypto"},
	}

	clusters := ClusterByTopic(markets)
	// p1 should appear in bitcoin cluster, ethereum cluster, and possibly combined
	// but each cluster should have it only once
	for _, c := range clusters {
		seen := map[string]bool{}
		for _, m := range c.Markets {
			if seen[m.VenueMarketID] {
				t.Errorf("duplicate market %s in cluster %s", m.VenueMarketID, c.Label)
			}
			seen[m.VenueMarketID] = true
		}
	}
}

func TestTopicClusterHasCrossVenue(t *testing.T) {
	tests := []struct {
		name    string
		markets []*models.CanonicalMarket
		want    bool
	}{
		{
			"empty",
			nil,
			false,
		},
		{
			"single market",
			[]*models.CanonicalMarket{{VenueID: models.VenuePolymarket}},
			false,
		},
		{
			"same venue",
			[]*models.CanonicalMarket{
				{VenueID: models.VenuePolymarket},
				{VenueID: models.VenuePolymarket},
			},
			false,
		},
		{
			"cross venue",
			[]*models.CanonicalMarket{
				{VenueID: models.VenuePolymarket},
				{VenueID: models.VenueKalshi},
			},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &TopicCluster{Markets: tt.markets}
			if got := c.HasCrossVenue(); got != tt.want {
				t.Errorf("HasCrossVenue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTopicClusterVenueBreakdown(t *testing.T) {
	c := &TopicCluster{
		Markets: []*models.CanonicalMarket{
			{VenueID: models.VenuePolymarket},
			{VenueID: models.VenuePolymarket},
			{VenueID: models.VenueKalshi},
		},
	}

	breakdown := c.VenueBreakdown()
	if breakdown[models.VenuePolymarket] != 2 {
		t.Errorf("expected 2 polymarket, got %d", breakdown[models.VenuePolymarket])
	}
	if breakdown[models.VenueKalshi] != 1 {
		t.Errorf("expected 1 kalshi, got %d", breakdown[models.VenueKalshi])
	}
}

func TestExtractNormKeyEntities(t *testing.T) {
	tests := []struct {
		title    string
		wantAny  []string // at least one of these should appear
	}{
		{"Will BTC hit $100k?", []string{"bitcoin"}},          // btc → bitcoin via synonyms
		{"Will the Fed cut rates?", nil}, // "fed" → "federal reserve" via synonyms, but extractNormKeyEntities works at word level
		{"Some random market", nil},                             // no topic keywords expected
	}

	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			entities := extractNormKeyEntities(tt.title)
			if tt.wantAny == nil {
				return // just testing no panic
			}
			found := false
			for _, want := range tt.wantAny {
				for _, got := range entities {
					if got == want {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("expected one of %v in entities %v", tt.wantAny, entities)
			}
		})
	}
}

func TestSignatureKeyDeterministic(t *testing.T) {
	sig := TopicSignature{Category: "crypto", Entities: []string{"bitcoin", "ethereum"}}
	k1 := signatureKey(sig)
	k2 := signatureKey(sig)
	if k1 != k2 {
		t.Errorf("signatureKey not deterministic: %q != %q", k1, k2)
	}
}

func TestSignatureLabel(t *testing.T) {
	tests := []struct {
		sig  TopicSignature
		want string
	}{
		{TopicSignature{Category: "crypto", Entities: []string{"bitcoin"}}, "Bitcoin (crypto)"},
		{TopicSignature{Category: "other", Entities: nil}, "general"},
		{TopicSignature{Category: "", Entities: []string{"trump"}}, "Trump"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := signatureLabel(tt.sig)
			if got != tt.want {
				t.Errorf("signatureLabel = %q, want %q", got, tt.want)
			}
		})
	}
}
