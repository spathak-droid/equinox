// Command equinox-ui serves a local web UI for searching and comparing
// equivalent prediction markets across Polymarket and Kalshi.
//
// Usage:
//
//	go run ./cmd/equinox-ui
//
// Then open http://localhost:8080 in your browser.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/storage"
	"github.com/equinox/internal/venues/kalshi"
	"github.com/equinox/internal/venues/polymarket"
	"github.com/joho/godotenv"
)

// browseCache caches the result of runIndexedBrowse when query=="" (browse all)
// so repeated requests don't re-compute matching. TTL is 5 minutes.
var browseCache struct {
	sync.Mutex
	data      *PageData
	expiresAt time.Time
}

func main() {
	// .env is optional -- Railway (and other cloud hosts) inject vars directly.
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	// Create venue clients once. The Kalshi client has no maxMarkets cap so
	// its events index covers all series. The events cache (5 min TTL) is
	// shared across all search requests served by this process.
	kalshiClient := kalshi.New(cfg.KalshiAPIKey, cfg.HTTPTimeout, cfg.KalshiSearchAPI)
	polyClient := polymarket.New(cfg.HTTPTimeout, cfg.PolymarketSearchAPI, uiPerVenueLimit)

	// Open SQLite index in background (optional -- enables /browse and /api/pairs).
	dbPath := os.Getenv("EQUINOX_DB")
	if dbPath == "" {
		dbPath = "equinox_markets_v2.db"
	}
	// Log DB file info for debugging deploy issues.
	if info, err := os.Stat(dbPath); err != nil {
		fmt.Printf("[equinox-ui] DB file %s: NOT FOUND (%v)\n", dbPath, err)
	} else {
		fmt.Printf("[equinox-ui] DB file %s: %.1f MB\n", dbPath, float64(info.Size())/(1024*1024))
	}

	var store atomic.Pointer[storage.Store]
	go func() {
		s, err := storage.NewStore(dbPath)
		if err != nil {
			fmt.Printf("[equinox-ui] WARNING: no index DB at %s: %v (browse disabled)\n", dbPath, err)
			return
		}
		store.Store(s)
		stats, _ := s.GetStats()
		fmt.Printf("[equinox-ui] Index loaded: %d markets (%d polymarket, %d kalshi)\n",
			stats.Total, stats.ByVenue[string(models.VenuePolymarket)], stats.ByVenue[string(models.VenueKalshi)])
	}()

	// Set up Qdrant + embedding clients for semantic search (optional).
	var qdrant *storage.QdrantClient
	var embedder *storage.EmbeddingClient
	if cfg.QdrantURL != "" && cfg.OpenAIAPIKey != "" {
		qdrant = storage.NewQdrantClient(cfg.QdrantURL, cfg.QdrantAPIKey, cfg.QdrantCollection)
		embedder = storage.NewEmbeddingClient(cfg.OpenAIAPIKey, cfg.EmbeddingModel)
		fmt.Printf("[equinox-ui] Qdrant semantic search enabled (%s)\n", cfg.QdrantURL)
	}

	fmt.Println("[equinox-ui] Serving at http://localhost:8080")

	// Serve embedded static assets (CSS, JS).
	staticSub, err := staticSubFS()
	if err != nil {
		log.Fatalf("loading static assets: %v", err)
	}
	http.Handle("/static/", securityHeaders(http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))))

	http.Handle("/", securityHeaders(handleIndex(cfg, kalshiClient, polyClient, &store, qdrant, embedder)))
	http.Handle("/stream", securityHeaders(handleStream(cfg, kalshiClient, polyClient)))
	http.Handle("/news", securityHeaders(handleNews(cfg)))
	http.Handle("/api/pairs", securityHeaders(handleAPIPairs(cfg, &store)))
	http.Handle("/api/stats", securityHeaders(handleAPIStats(&store)))

	// Periodically evict expired entries from the result cache.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			resultCache.Range(func(key, value any) bool {
				if cr, ok := value.(*cachedResult); ok && now.After(cr.expiresAt) {
					resultCache.Delete(key)
				}
				return true
			})
		}
	}()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("[equinox-ui] Listening on :%s\n", port)
	server := &http.Server{
		Addr:         ":" + port,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}

// securityHeaders wraps an http.Handler and sets security response headers.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=()")
		next.ServeHTTP(w, r)
	})
}


// handleIndex serves the main page with optional cached results.
func handleIndex(cfg *config.Config, kalshiClient *kalshi.Client, polyClient *polymarket.Client, storePtr *atomic.Pointer[storage.Store], qdrant *storage.QdrantClient, embedder *storage.EmbeddingClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		query := truncateQuery(strings.TrimSpace(r.URL.Query().Get("q")), 500)
		browse := r.URL.Query().Get("browse") == "1"

		store := storePtr.Load()

		var data *PageData
		var err error

		if browse && store != nil {
			// Browse mode: use indexed pairs from SQLite
			limit := parseLimitParam(r, 50, 200)
			data, err = runIndexedBrowse(cfg, store, query, limit)
			if err != nil {
				log.Printf("runIndexedBrowse (browse): %v", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
		} else if query == "" {
			data = &PageData{VenueCounts: map[models.VenueID]int{}}
			if store != nil {
				if stats, err := store.GetStats(); err == nil {
					data.IndexStats = &IndexStats{
						Total:      stats.Total,
						Polymarket: stats.ByVenue[string(models.VenuePolymarket)],
						Kalshi:     stats.ByVenue[string(models.VenueKalshi)],
						LastUpdate: stats.LastUpdate,
					}
				}
			} else {
				data.IndexLoading = true
			}
		} else if qdrant != nil && embedder != nil && store != nil {
			// Qdrant semantic search with short timeout, FTS fallback
			qCtx, qCancel := context.WithTimeout(r.Context(), 10*time.Second)
			data, err = runQdrantSearch(qCtx, cfg, kalshiClient, qdrant, embedder, store, query, 20)
			qCancel()
			if err != nil {
				fmt.Printf("[equinox-ui] Qdrant failed, falling back to FTS: %v\n", err)
				data, err = runIndexedBrowse(cfg, store, query, 50)
				if err != nil {
					log.Printf("runIndexedBrowse (fallback): %v", err)
					http.Error(w, "Internal server error", http.StatusInternalServerError)
					return
				}
			}
			data.IndexSearch = true
			data.BrowseMode = false
		} else if store != nil {
			// FTS search from SQLite (instant, always available when index is loaded)
			limit := parseLimitParam(r, 50, 200)
			data, err = runIndexedBrowse(cfg, store, query, limit)
			if err != nil {
				log.Printf("runIndexedBrowse (FTS): %v", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			data.IndexSearch = true
			data.BrowseMode = false
		} else {
			// No index: render page with LiveSearchPending so JS auto-triggers SSE
			data = &PageData{
				SearchQuery:       query,
				HasQuery:          true,
				LiveSearchPending: true,
				VenueCounts:       map[models.VenueID]int{},
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := pageTmpl.Execute(w, data); err != nil {
			fmt.Printf("[equinox-ui] ERROR: rendering template: %v\n", err)
		}
	}
}

// handleStream runs the search pipeline and emits SSE progress events.
func handleStream(cfg *config.Config, kalshiClient *kalshi.Client, polyClient *polymarket.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := truncateQuery(strings.TrimSpace(r.URL.Query().Get("q")), 500)
		deepSearch := r.URL.Query().Get("more") == "1"
		cacheKey := query
		if deepSearch {
			cacheKey = query + "|more=1"
		}
		if query == "" {
			http.Error(w, "missing q", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		var mu sync.Mutex
		emit := func(evt progressEvent) {
			b, _ := json.Marshal(evt)
			mu.Lock()
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
			mu.Unlock()
		}

		data, pipelineErr := runSearchPipelineWithProgress(cfg, kalshiClient, polyClient, query, deepSearch, emit)
		if pipelineErr != nil {
			log.Printf("search pipeline error: %v", pipelineErr)
			emit(progressEvent{Type: "error", Msg: "Search failed. Please try again."})
			return
		}

		// Cache result so the redirect GET / is instant
		resultCache.Store(cacheKey, &cachedResult{
			data:      data,
			expiresAt: time.Now().Add(2 * time.Minute),
		})
		emit(progressEvent{Type: "done"})
	}
}
