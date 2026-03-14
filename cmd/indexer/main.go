// Command indexer fetches ALL open markets from Polymarket and Kalshi
// and stores them in a local SQLite database with FTS5 full-text search.
//
// Usage:
//
//	go run ./cmd/indexer                     # index all open markets
//	go run ./cmd/indexer -db markets.db      # custom DB path
//	go run ./cmd/indexer -venue polymarket   # index only one venue
//	go run ./cmd/indexer -purge 24h          # remove markets not seen in 24h
//	go run ./cmd/indexer -stats              # show DB statistics
package main

import (
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/normalizer"
	"github.com/equinox/internal/storage"
	"github.com/equinox/internal/venues"
	"github.com/equinox/internal/venues/kalshi"
	"github.com/equinox/internal/venues/polymarket"
	"github.com/joho/godotenv"
)

func main() {
	dbPath := flag.String("db", "equinox_markets.db", "path to SQLite database")
	venueFilter := flag.String("venue", "", "index only this venue (polymarket, kalshi)")
	purgeAge := flag.String("purge", "", "purge markets not updated within this duration (e.g. 24h, 48h)")
	statsOnly := flag.Bool("stats", false, "show database statistics and exit")
	rebuildFTS := flag.Bool("rebuild-fts", false, "rebuild FTS index from existing data")
	rebuildVectors := flag.Bool("rebuild-vectors", false, "re-embed all markets from SQLite into Qdrant")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall timeout for indexing")
	flag.Parse()

	_ = godotenv.Load()

	store, err := storage.NewStore(*dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer store.Close()

	if *statsOnly {
		printStats(store)
		return
	}

	if *rebuildFTS {
		if err := store.RebuildFTS(); err != nil {
			log.Fatalf("Failed to rebuild FTS: %v", err)
		}
		printStats(store)
		return
	}

	if *purgeAge != "" {
		doPurge(store, *purgeAge)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Set up Qdrant + embedding clients if configured
	var qdrant *storage.QdrantClient
	var embedder *storage.EmbeddingClient
	if cfg.QdrantURL != "" && cfg.OpenAIAPIKey != "" {
		qdrant = storage.NewQdrantClient(cfg.QdrantURL, cfg.QdrantAPIKey, cfg.QdrantCollection)
		embedder = storage.NewEmbeddingClient(cfg.OpenAIAPIKey, cfg.EmbeddingModel)
		if err := qdrant.EnsureCollection(ctx, embedder.VectorDimension()); err != nil {
			fmt.Fprintf(os.Stderr, "[indexer] WARNING: Qdrant setup failed: %v (continuing without vectors)\n", err)
			qdrant = nil
			embedder = nil
		}
	} else {
		fmt.Println("[indexer] Qdrant disabled (set QDRANT_URL + OPENAI_API_KEY to enable)")
	}

	if *rebuildVectors {
		if qdrant == nil || embedder == nil {
			log.Fatalf("--rebuild-vectors requires QDRANT_URL + OPENAI_API_KEY")
		}
		rebuildAllVectors(ctx, store, qdrant, embedder)
		return
	}

	norm := normalizer.New(cfg)
	start := time.Now()

	venue := strings.ToLower(*venueFilter)
	if venue == "" || venue == "polymarket" {
		indexPolymarket(ctx, cfg, norm, store, qdrant, embedder)
	}
	if venue == "" || venue == "kalshi" {
		indexKalshi(ctx, cfg, norm, store, qdrant, embedder)
	}

	// Rebuild FTS index after all markets are stored (much faster than per-row updates)
	if err := store.RebuildFTS(); err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] WARNING: FTS rebuild failed: %v\n", err)
	}

	elapsed := time.Since(start)
	fmt.Printf("\n=== Indexing complete in %s ===\n", elapsed.Round(time.Second))
	printStats(store)
}

func indexPolymarket(ctx context.Context, cfg *config.Config, norm *normalizer.Normalizer, store *storage.Store, qdrant *storage.QdrantClient, embedder *storage.EmbeddingClient) {
	fmt.Println("\n── Indexing Polymarket (all open markets) ──")

	// Create client with no market limit (0 = unlimited)
	client := polymarket.New(cfg.HTTPTimeout, cfg.PolymarketSearchAPI, 0)
	raw, err := client.FetchMarkets(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] ERROR fetching Polymarket: %v\n", err)
		return
	}
	fmt.Printf("[indexer] Polymarket: fetched %d raw markets\n", len(raw))

	storeNormalized(ctx, norm, store, qdrant, embedder, raw, "polymarket")
}

func indexKalshi(ctx context.Context, cfg *config.Config, norm *normalizer.Normalizer, store *storage.Store, qdrant *storage.QdrantClient, embedder *storage.EmbeddingClient) {
	fmt.Println("\n── Indexing Kalshi (all open markets via v2 API) ──")

	client := kalshi.New(cfg.KalshiAPIKey, cfg.HTTPTimeout, cfg.KalshiSearchAPI)
	raw, err := client.FetchAllOpenMarkets(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] ERROR fetching Kalshi: %v\n", err)
		return
	}
	fmt.Printf("[indexer] Kalshi: fetched %d raw markets\n", len(raw))

	// Enrich v2 markets with images + series_ticker from v1 search API
	client.EnrichWithV1Data(ctx, raw)

	storeNormalized(ctx, norm, store, qdrant, embedder, raw, "kalshi")
}

func storeNormalized(ctx context.Context, norm *normalizer.Normalizer, store *storage.Store, qdrant *storage.QdrantClient, embedder *storage.EmbeddingClient, raw []*venues.RawMarket, venue string) {
	markets, err := norm.Normalize(ctx, raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] ERROR normalizing %s: %v\n", venue, err)
		return
	}
	fmt.Printf("[indexer] %s: normalized %d markets (skipped %d malformed)\n",
		venue, len(markets), len(raw)-len(markets))

	inserted, _, err := store.UpsertMarkets(markets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] ERROR storing %s: %v\n", venue, err)
		return
	}
	fmt.Printf("[indexer] %s: upserted %d markets into database\n", venue, inserted)

	// Embed and upsert to Qdrant
	if qdrant != nil && embedder != nil {
		upsertVectors(ctx, qdrant, embedder, markets, venue)
	}
}

// upsertVectors embeds market titles and upserts vectors to Qdrant in batches.
func upsertVectors(ctx context.Context, qdrant *storage.QdrantClient, embedder *storage.EmbeddingClient, markets []*models.CanonicalMarket, venue string) {
	const batchSize = 100
	total := 0

	for i := 0; i < len(markets); i += batchSize {
		end := i + batchSize
		if end > len(markets) {
			end = len(markets)
		}
		batch := markets[i:end]

		// Filter out garbage markets: combo/parlay titles, very short titles, bracket outcomes, etc.
		var filtered []*models.CanonicalMarket
		for _, m := range batch {
			// Skip combo/parlay markets with outcome-string titles
			if strings.HasPrefix(m.Title, "yes ") || strings.HasPrefix(m.Title, "no ") {
				continue
			}
			// Skip markets with very short or empty titles
			if len(m.Title) < 5 {
				continue
			}
			// Skip bracket/range outcomes: "Bitcoin price range? — $63,000 to $63,999"
			if idx := strings.Index(m.Title, " — "); idx >= 0 {
				suffix := strings.TrimSpace(m.Title[idx+len(" — "):])
				if len(suffix) > 0 && (suffix[0] == '$' || (suffix[0] >= '0' && suffix[0] <= '9')) {
					continue
				}
			}
			filtered = append(filtered, m)
		}
		if len(filtered) == 0 {
			continue
		}

		// Build texts: title + subtitle + category for richer embeddings
		texts := make([]string, len(filtered))
		for j, m := range filtered {
			text := m.Title
			if m.Subtitle != "" && !strings.HasPrefix(m.Subtitle, "yes ") && !strings.HasPrefix(m.Subtitle, "no ") {
				text += " — " + m.Subtitle
			}
			if m.Category != "" && m.Category != "other" {
				text += " [" + m.Category + "]"
			}
			texts[j] = text
		}

		vecs, err := embedder.EmbedBatch(ctx, texts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[indexer] WARNING: embedding batch %d-%d failed: %v\n", i, end, err)
			continue
		}

		points := make([]storage.QdrantPoint, 0, len(filtered))
		for j, m := range filtered {
			if j >= len(vecs) || vecs[j] == nil {
				continue
			}
			// Generate deterministic UUID from venue:market_id (Qdrant requires UUID or uint)
			pointID := deterministicUUID(string(m.VenueID), m.VenueMarketID)
			payload := map[string]any{
				"venue_id":        string(m.VenueID),
				"venue_market_id": m.VenueMarketID,
				"title":           m.Title,
				"subtitle":        m.Subtitle,
				"category":        m.Category,
				"yes_price":       m.YesPrice,
				"no_price":        m.NoPrice,
				"liquidity":       m.Liquidity,
				"volume_24h":      m.Volume24h,
				"status":          string(m.Status),
			}
			points = append(points, storage.QdrantPoint{
				ID:      pointID,
				Vector:  vecs[j],
				Payload: payload,
			})
		}

		if err := qdrant.UpsertPoints(ctx, points); err != nil {
			fmt.Fprintf(os.Stderr, "[indexer] WARNING: Qdrant upsert batch %d-%d failed: %v\n", i, end, err)
			continue
		}
		total += len(points)
		fmt.Printf("[indexer] %s: embedded + upserted %d/%d to Qdrant\n", venue, total, len(markets))
	}
}

// rebuildAllVectors reads all markets from SQLite and re-embeds into Qdrant.
func rebuildAllVectors(ctx context.Context, store *storage.Store, qdrant *storage.QdrantClient, embedder *storage.EmbeddingClient) {
	fmt.Println("\n── Rebuilding all vectors in Qdrant ──")

	for _, venue := range []string{"polymarket", "kalshi"} {
		markets, err := store.GetTopMarketsLite(venue, 500000)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[indexer] ERROR loading %s markets: %v\n", venue, err)
			continue
		}
		fmt.Printf("[indexer] %s: loaded %d markets from SQLite\n", venue, len(markets))
		upsertVectors(ctx, qdrant, embedder, markets, venue)
	}

	info, err := qdrant.GetCollectionInfo(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] WARNING: could not get Qdrant stats: %v\n", err)
		return
	}
	fmt.Printf("\n=== Qdrant: %d vectors in collection ===\n", info.PointsCount)
}

// deterministicUUID generates a UUID v3-style ID from venue + market ID.
func deterministicUUID(venueID, marketID string) string {
	h := md5.Sum([]byte(venueID + ":" + marketID))
	// Set version 3 and variant bits
	h[6] = (h[6] & 0x0f) | 0x30
	h[8] = (h[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func printStats(store *storage.Store) {
	stats, err := store.GetStats()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[indexer] ERROR getting stats: %v\n", err)
		return
	}

	fmt.Println("\n=== Database Statistics ===")
	fmt.Printf("  Total active markets: %d\n", stats.Total)
	for venue, count := range stats.ByVenue {
		fmt.Printf("  %-15s %d\n", venue+":", count)
	}
	if !stats.LastUpdate.IsZero() {
		fmt.Printf("  Last updated:        %s\n", stats.LastUpdate.Format(time.RFC3339))
	}
}

func doPurge(store *storage.Store, ageStr string) {
	dur, err := time.ParseDuration(ageStr)
	if err != nil {
		log.Fatalf("Invalid purge duration %q: %v", ageStr, err)
	}

	cutoff := time.Now().UTC().Add(-dur)
	removed, err := store.PurgeStale(cutoff)
	if err != nil {
		log.Fatalf("Purge failed: %v", err)
	}
	fmt.Printf("[indexer] Purged %d markets not updated since %s\n", removed, cutoff.Format(time.RFC3339))
	printStats(store)
}
