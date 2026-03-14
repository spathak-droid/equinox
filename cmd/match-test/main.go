package main

import (
	"fmt"
	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
)

func main() {
	cfg, _ := config.Load()
	m := matcher.New(cfg)

	polymarket := &models.CanonicalMarket{
		VenueID:       "polymarket",
		VenueMarketID: "pm-trump",
		Title:         "Will Donald Trump win the 2028 US Presidential Election?",
		Status:        models.StatusActive,
	}
	kalshi := &models.CanonicalMarket{
		VenueID:       "kalshi",
		VenueMarketID: "kx-trump",
		Title:         "2028 U.S. Presidential Election winner? — Donald J. Trump",
		Status:        models.StatusActive,
	}

	// Test RankCandidatesByBestMatch (what Qdrant path uses)
	ranked := matcher.RankCandidatesByBestMatch(
		[]*models.CanonicalMarket{polymarket},
		[]*models.CanonicalMarket{kalshi},
		20,
	)
	fmt.Printf("RankCandidatesByBestMatch: %d results\n", len(ranked))
	for i, r := range ranked {
		fmt.Printf("  [%d] %q vs %q\n", i, r.Source.Title, r.Candidate.Title)
	}

	// Test FindMatchesFromSearchResults (what FTS path uses)
	results := m.FindMatchesFromSearchResults([]matcher.SearchResult{
		{Source: polymarket, Candidates: []*models.CanonicalMarket{kalshi}},
	}, 5)
	fmt.Printf("\nFindMatchesFromSearchResults: %d results\n", len(results))
	for _, r := range results {
		fmt.Printf("  confidence=%s score=%.3f fuzzy=%.3f\n  explanation=%s\n",
			r.Confidence, r.CompositeScore, r.FuzzyScore, r.Explanation)
	}

	// Test with more Poly candidates
	candidates := []struct{ name, title string }{
		{"Marco Rubio", "Will Marco Rubio win the 2028 US Presidential Election?"},
		{"Kamala Harris", "Will Kamala Harris win the 2028 US Presidential Election?"},
		{"Gavin Newsom", "Will Gavin Newsom win the 2028 US Presidential Election?"},
	}
	kalshiCandidates := []struct{ name, title string }{
		{"Marco Rubio", "2028 U.S. Presidential Election winner? — Marco Rubio"},
		{"Kamala Harris", "2028 U.S. Presidential Election winner? — Kamala Harris"},
		{"Gavin Newsom", "2028 U.S. Presidential Election winner? — Gavin Newsom"},
	}

	var polyAll, kalshiAll []*models.CanonicalMarket
	for _, c := range candidates {
		polyAll = append(polyAll, &models.CanonicalMarket{
			VenueID: "polymarket", VenueMarketID: "pm-" + c.name, Title: c.title, Status: models.StatusActive,
		})
	}
	for _, c := range kalshiCandidates {
		kalshiAll = append(kalshiAll, &models.CanonicalMarket{
			VenueID: "kalshi", VenueMarketID: "kx-" + c.name, Title: c.title, Status: models.StatusActive,
		})
	}

	ranked2 := matcher.RankCandidatesByBestMatch(polyAll, kalshiAll, 20)
	fmt.Printf("\nBatch RankCandidatesByBestMatch: %d results\n", len(ranked2))
	for i, r := range ranked2 {
		fmt.Printf("  [%d] %q vs %q\n", i, r.Source.Title, r.Candidate.Title)
	}
}
