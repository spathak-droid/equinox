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
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/news"
	"github.com/equinox/internal/venues/kalshi"
	"github.com/equinox/internal/venues/polymarket"
	"github.com/joho/godotenv"
)

func main() {
	// .env is optional — Railway (and other cloud hosts) inject vars directly.
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

	fmt.Println("[equinox-ui] Serving at http://localhost:8080")

	// Serve embedded static assets (CSS, JS).
	staticSub, err := staticSubFS()
	if err != nil {
		log.Fatalf("loading static assets: %v", err)
	}
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	http.HandleFunc("/", handleIndex(cfg, kalshiClient, polyClient))
	http.HandleFunc("/stream", handleStream(cfg, kalshiClient, polyClient))
	http.HandleFunc("/news", handleNews(cfg))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("[equinox-ui] Listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// handleIndex serves the main page with optional cached results.
func handleIndex(cfg *config.Config, kalshiClient *kalshi.Client, polyClient *polymarket.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		deepSearch := r.URL.Query().Get("more") == "1"
		cacheKey := query
		if deepSearch {
			cacheKey = query + "|more=1"
		}

		var data *PageData
		var err error
		if query == "" {
			data = &PageData{VenueCounts: map[models.VenueID]int{}}
		} else {
			// Check short-lived cache populated by /stream
			if v, ok := resultCache.Load(cacheKey); ok {
				cr := v.(*cachedResult)
				if time.Now().Before(cr.expiresAt) {
					data = cr.data
				} else {
					resultCache.Delete(cacheKey)
				}
			}
			if data == nil {
				fmt.Printf("[equinox-ui] Running search pipeline q=%q deep=%t\n", query, deepSearch)
				data, err = runSearchPipelineWithProgress(cfg, kalshiClient, polyClient, query, deepSearch, nil)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
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
		query := strings.TrimSpace(r.URL.Query().Get("q"))
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
			emit(progressEvent{Type: "error", Msg: pipelineErr.Error()})
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

// handleNews returns news articles for a query as JSON.
func handleNews(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		if query == "" {
			http.Error(w, "missing q", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		fetcher := news.NewFetcher(cfg.HTTPTimeout, cfg.NewsMaxArticles)
		mn := fetcher.FetchForQuery(ctx, query)
		var articles []NewsArticleView
		if mn != nil {
			articles = toNewsArticleViews(mn)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		json.NewEncoder(w).Encode(map[string]any{
			"query":    query,
			"articles": articles,
		})
	}
}
