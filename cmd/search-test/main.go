// Quick test tool to validate FTS search against the indexed market database.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/equinox/internal/storage"
)

func main() {
	dbPath := "equinox_markets.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	store, err := storage.NewStore(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	queries := []struct {
		query        string
		excludeVenue string
	}{
		{"Bitcoin", ""},
		{"Bitcoin", "polymarket"},  // Kalshi only
		{"Bitcoin", "kalshi"},      // Polymarket only
		{"Trump president", ""},
		{"Fed interest rate", ""},
		{"NBA finals", ""},
		{"Ethereum price", ""},
		{"Ukraine Russia ceasefire", ""},
		{"S&P 500", ""},
		{"World Cup", ""},
	}

	for _, q := range queries {
		results, err := store.SearchByTitle(q.query, q.excludeVenue, 5)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			continue
		}
		venue := "all"
		if q.excludeVenue != "" {
			venue = "excl:" + q.excludeVenue
		}
		fmt.Printf("\n=== %q [%s] → %d results ===\n", q.query, venue, len(results))
		for i, m := range results {
			fmt.Printf("  %d. [%s] %s (yes=%.2f spread=%.4f vol24h=$%.0f)\n",
				i+1, m.VenueID, m.Title, m.YesPrice, m.Spread, m.Volume24h)
		}
	}
}
