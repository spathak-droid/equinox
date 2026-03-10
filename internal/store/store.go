// Package store provides SQLite-backed persistence for cached pipeline results.
// The homepage serves data from this store, and a background worker refreshes it
// on a configurable interval (default 10 minutes).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const defaultDBPath = ".equinox_cache.db"

// Store wraps a SQLite database for caching pipeline results.
type Store struct {
	db *sql.DB
}

// CachedPageData is the JSON-serializable snapshot stored in SQLite.
type CachedPageData struct {
	RunAt        string            `json:"run_at"`
	TotalMarkets int               `json:"total_markets"`
	VenueCounts  map[string]int    `json:"venue_counts"`
	Pairs        json.RawMessage   `json:"pairs"`
	MatchCount   int               `json:"match_count"`
	ProbableCount int              `json:"probable_count"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// Open opens (or creates) the SQLite database at the given path.
// Pass "" to use the default path.
func Open(path string) (*Store, error) {
	if path == "" {
		path = defaultDBPath
	}
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS cache (
			key        TEXT PRIMARY KEY,
			data       TEXT NOT NULL,
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)
	`)
	return err
}

// SaveHomepage persists the homepage data as a JSON blob.
func (s *Store) SaveHomepage(data *CachedPageData) error {
	data.UpdatedAt = time.Now()
	blob, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO cache (key, data, updated_at) VALUES ('homepage', ?, datetime('now'))`,
		string(blob),
	)
	return err
}

// LoadHomepage retrieves the cached homepage data. Returns nil, nil if no cache exists.
func (s *Store) LoadHomepage() (*CachedPageData, error) {
	var blob string
	err := s.db.QueryRow(`SELECT data FROM cache WHERE key = 'homepage'`).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: load homepage: %w", err)
	}
	var data CachedPageData
	if err := json.Unmarshal([]byte(blob), &data); err != nil {
		return nil, fmt.Errorf("store: unmarshal homepage: %w", err)
	}
	return &data, nil
}

// Age returns how long ago the homepage cache was last updated.
// Returns a very large duration if no cache exists.
func (s *Store) Age() time.Duration {
	var updatedAt string
	err := s.db.QueryRow(`SELECT updated_at FROM cache WHERE key = 'homepage'`).Scan(&updatedAt)
	if err != nil {
		return 24 * time.Hour // treat as very stale
	}
	t, err := time.Parse("2006-01-02 15:04:05", updatedAt)
	if err != nil {
		return 24 * time.Hour
	}
	return time.Since(t)
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}
