package models

import (
	"testing"
	"time"
)

func TestHasResolutionDate(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		m    *CanonicalMarket
		want bool
	}{
		{"with date", &CanonicalMarket{ResolutionDate: &now}, true},
		{"nil date", &CanonicalMarket{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.HasResolutionDate(); got != tt.want {
				t.Errorf("HasResolutionDate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeCategory(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"politics", "politics"},
		{"election", "politics"},
		{"government", "politics"},
		{"economics", "economics"},
		{"economy", "economics"},
		{"finance", "economics"},
		{"markets", "economics"},
		{"crypto", "crypto"},
		{"bitcoin", "crypto"},
		{"ethereum", "crypto"},
		{"sports", "sports"},
		{"nba", "sports"},
		{"nfl", "sports"},
		{"soccer", "sports"},
		{"science", "science"},
		{"technology", "technology"},
		{"tech", "technology"},
		{"ai", "technology"},
		{"geopolitics", "geopolitics"},
		{"world", "geopolitics"},
		{"unknown", "other"},
		{"", "other"},
		{"random-category", "other"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := NormalizeCategory(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeCategory(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMarketStatusConstants(t *testing.T) {
	// Verify status constants are distinct
	statuses := []MarketStatus{StatusActive, StatusClosed, StatusResolved, StatusUnknown}
	seen := map[MarketStatus]bool{}
	for _, s := range statuses {
		if seen[s] {
			t.Errorf("duplicate status: %s", s)
		}
		seen[s] = true
	}
}

func TestVenueIDConstants(t *testing.T) {
	if VenuePolymarket == VenueKalshi {
		t.Error("venue IDs should be distinct")
	}
	if VenuePolymarket == "" || VenueKalshi == "" {
		t.Error("venue IDs should not be empty")
	}
}
