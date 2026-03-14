// Package storage provides SQLite-backed persistence for indexed markets.
// It uses FTS5 for full-text search over market titles, enabling fast
// cross-venue candidate discovery without hitting external APIs.
package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed market store with FTS5 search.
type Store struct {
	db *sql.DB
}

// Stats holds per-venue market counts.
type Stats struct {
	Total      int
	ByVenue    map[string]int
	LastUpdate time.Time
}

// NewStore opens (or creates) a SQLite database at the given path.
func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}

	// Set pragmas for performance and concurrency
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=-64000", // 64MB cache
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting %s: %w", p, err)
		}
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging sqlite: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating sqlite: %w", err)
	}
	return s, nil
}

// migrate creates the schema and indexes if they don't exist.
func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS markets (
			rowid INTEGER PRIMARY KEY AUTOINCREMENT,
			venue_id TEXT NOT NULL,
			venue_market_id TEXT NOT NULL,
			venue_event_ticker TEXT DEFAULT '',
			venue_series_ticker TEXT DEFAULT '',
			venue_event_title TEXT DEFAULT '',
			venue_slug TEXT DEFAULT '',
			title TEXT NOT NULL,
			subtitle TEXT DEFAULT '',
			description TEXT DEFAULT '',
			category TEXT DEFAULT '',
			tags TEXT DEFAULT '',
			image_url TEXT DEFAULT '',
			resolution_date TEXT,
			yes_price REAL DEFAULT 0,
			no_price REAL DEFAULT 0,
			spread REAL DEFAULT 0,
			volume_24h REAL DEFAULT 0,
			open_interest REAL DEFAULT 0,
			liquidity REAL DEFAULT 0,
			status TEXT DEFAULT 'active',
			raw_payload TEXT,
			indexed_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(venue_id, venue_market_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_markets_venue ON markets(venue_id)`,
		`CREATE INDEX IF NOT EXISTS idx_markets_status ON markets(status)`,
		`CREATE INDEX IF NOT EXISTS idx_markets_category ON markets(category)`,
		`CREATE INDEX IF NOT EXISTS idx_markets_status_venue ON markets(status, venue_id)`,
		`CREATE INDEX IF NOT EXISTS idx_markets_updated_at ON markets(updated_at)`,
		// FTS5 virtual table for title search (standalone, not external content)
		`CREATE VIRTUAL TABLE IF NOT EXISTS markets_fts USING fts5(
			title,
			subtitle,
			description,
			venue_id,
			venue_market_id,
			tokenize='porter unicode61'
		)`,
		// Drop old triggers from previous schema (no longer used)
		`DROP TRIGGER IF EXISTS markets_ai`,
		`DROP TRIGGER IF EXISTS markets_ad`,
		`DROP TRIGGER IF EXISTS markets_au`,
	}

	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			// If FTS table creation fails (schema mismatch from old version),
			// drop and retry once.
			if strings.Contains(stmt, "markets_fts") && strings.Contains(stmt, "CREATE") {
				s.db.Exec(`DROP TABLE IF EXISTS markets_fts`)
				if _, err2 := s.db.Exec(stmt); err2 != nil {
					return fmt.Errorf("exec (retry) %q: %w", stmt[:60], err2)
				}
				continue
			}
			return fmt.Errorf("exec %q: %w", stmt[:60], err)
		}
	}
	return nil
}

// GetStats returns index statistics.
func (s *Store) GetStats() (Stats, error) {
	stats := Stats{ByVenue: make(map[string]int)}

	rows, err := s.db.Query(`SELECT venue_id, COUNT(*) FROM markets WHERE status = 'active' GROUP BY venue_id`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()

	for rows.Next() {
		var venue string
		var count int
		if err := rows.Scan(&venue, &count); err != nil {
			return stats, err
		}
		stats.ByVenue[venue] = count
		stats.Total += count
	}

	var lastUpdate sql.NullString
	s.db.QueryRow(`SELECT MAX(updated_at) FROM markets`).Scan(&lastUpdate)
	if lastUpdate.Valid {
		stats.LastUpdate, _ = time.Parse(time.RFC3339, lastUpdate.String)
	}

	return stats, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
