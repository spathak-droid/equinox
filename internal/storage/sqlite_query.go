package storage

import (
	"database/sql"
	"fmt"

	"github.com/equinox/internal/models"
)

// GetMarket retrieves a single market by venue_id and venue_market_id.
func (s *Store) GetMarket(venueID, venueMarketID string) (*models.CanonicalMarket, error) {
	rows, err := s.db.Query(`
		SELECT venue_id, venue_market_id, venue_event_ticker, venue_series_ticker,
			venue_event_title, venue_slug, title, subtitle, description,
			category, tags, image_url, resolution_date,
			yes_price, no_price, spread,
			volume_24h, open_interest, liquidity,
			status, raw_payload
		FROM markets
		WHERE venue_id = ? AND venue_market_id = ?
		LIMIT 1`, venueID, venueMarketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	markets, err := scanMarkets(rows)
	if err != nil {
		return nil, err
	}
	if len(markets) == 0 {
		return nil, nil
	}
	return markets[0], nil
}

// SearchByTitle finds markets matching a text query using FTS5.
// If excludeVenue is non-empty, results from that venue are excluded
// (useful for cross-venue candidate discovery).
func (s *Store) SearchByTitle(query string, excludeVenue string, limit int) ([]*models.CanonicalMarket, error) {
	if limit <= 0 {
		limit = 50
	}

	// Clean query for FTS5: escape special chars
	ftsQuery := sanitizeFTSQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}

	var rows *sql.Rows
	var err error

	if excludeVenue != "" {
		rows, err = s.db.Query(`
			SELECT m.venue_id, m.venue_market_id, m.venue_event_ticker, m.venue_series_ticker,
				m.venue_event_title, m.venue_slug, m.title, m.subtitle, m.description,
				m.category, m.tags, m.image_url, m.resolution_date,
				m.yes_price, m.no_price, m.spread,
				m.volume_24h, m.open_interest, m.liquidity,
				m.status, m.raw_payload
			FROM markets_fts fts
			JOIN markets m ON m.venue_id = fts.venue_id AND m.venue_market_id = fts.venue_market_id
			WHERE markets_fts MATCH ?
				AND fts.venue_id != ?
				AND m.status = 'active'
			ORDER BY rank * (1.0 + ln(1.0 + m.volume_24h + m.liquidity))
			LIMIT ?`,
			ftsQuery, excludeVenue, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT m.venue_id, m.venue_market_id, m.venue_event_ticker, m.venue_series_ticker,
				m.venue_event_title, m.venue_slug, m.title, m.subtitle, m.description,
				m.category, m.tags, m.image_url, m.resolution_date,
				m.yes_price, m.no_price, m.spread,
				m.volume_24h, m.open_interest, m.liquidity,
				m.status, m.raw_payload
			FROM markets_fts fts
			JOIN markets m ON m.venue_id = fts.venue_id AND m.venue_market_id = fts.venue_market_id
			WHERE markets_fts MATCH ?
				AND m.status = 'active'
			ORDER BY rank * (1.0 + ln(1.0 + m.volume_24h + m.liquidity))
			LIMIT ?`,
			ftsQuery, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()

	return scanMarkets(rows)
}

// SearchByTitleOR is like SearchByTitle but uses OR between words instead of AND.
// This is more forgiving for cross-venue matching where titles differ significantly.
func (s *Store) SearchByTitleOR(query string, excludeVenue string, limit int) ([]*models.CanonicalMarket, error) {
	if limit <= 0 {
		limit = 50
	}

	ftsQuery := sanitizeFTSQueryOR(query)
	if ftsQuery == "" {
		return nil, nil
	}

	var rows *sql.Rows
	var err error

	if excludeVenue != "" {
		rows, err = s.db.Query(`
			SELECT m.venue_id, m.venue_market_id, m.venue_event_ticker, m.venue_series_ticker,
				m.venue_event_title, m.venue_slug, m.title, m.subtitle, m.description,
				m.category, m.tags, m.image_url, m.resolution_date,
				m.yes_price, m.no_price, m.spread,
				m.volume_24h, m.open_interest, m.liquidity,
				m.status, m.raw_payload
			FROM markets_fts fts
			JOIN markets m ON m.venue_id = fts.venue_id AND m.venue_market_id = fts.venue_market_id
			WHERE markets_fts MATCH ?
				AND fts.venue_id != ?
				AND m.status = 'active'
			ORDER BY rank * (1.0 + ln(1.0 + m.volume_24h + m.liquidity))
			LIMIT ?`,
			ftsQuery, excludeVenue, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT m.venue_id, m.venue_market_id, m.venue_event_ticker, m.venue_series_ticker,
				m.venue_event_title, m.venue_slug, m.title, m.subtitle, m.description,
				m.category, m.tags, m.image_url, m.resolution_date,
				m.yes_price, m.no_price, m.spread,
				m.volume_24h, m.open_interest, m.liquidity,
				m.status, m.raw_payload
			FROM markets_fts fts
			JOIN markets m ON m.venue_id = fts.venue_id AND m.venue_market_id = fts.venue_market_id
			WHERE markets_fts MATCH ?
				AND m.status = 'active'
			ORDER BY rank * (1.0 + ln(1.0 + m.volume_24h + m.liquidity))
			LIMIT ?`,
			ftsQuery, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("fts search (OR): %w", err)
	}
	defer rows.Close()

	return scanMarkets(rows)
}

// GetAllMarkets returns all active markets, optionally filtered by venue.
func (s *Store) GetAllMarkets(venue string) ([]*models.CanonicalMarket, error) {
	var rows *sql.Rows
	var err error

	if venue != "" {
		rows, err = s.db.Query(`
			SELECT venue_id, venue_market_id, venue_event_ticker, venue_series_ticker,
				venue_event_title, venue_slug, title, subtitle, description,
				category, tags, image_url, resolution_date,
				yes_price, no_price, spread,
				volume_24h, open_interest, liquidity,
				status, raw_payload
			FROM markets WHERE status = 'active' AND venue_id = ?
			ORDER BY volume_24h DESC`, venue)
	} else {
		rows, err = s.db.Query(`
			SELECT venue_id, venue_market_id, venue_event_ticker, venue_series_ticker,
				venue_event_title, venue_slug, title, subtitle, description,
				category, tags, image_url, resolution_date,
				yes_price, no_price, spread,
				volume_24h, open_interest, liquidity,
				status, raw_payload
			FROM markets WHERE status = 'active'
			ORDER BY volume_24h DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanMarkets(rows)
}

// GetTopMarkets returns the top N markets by volume for a specific venue.
// Much faster than GetAllMarkets when you only need a subset.
func (s *Store) GetTopMarkets(venue string, limit int) ([]*models.CanonicalMarket, error) {
	rows, err := s.db.Query(`
		SELECT venue_id, venue_market_id, venue_event_ticker, venue_series_ticker,
			venue_event_title, venue_slug, title, subtitle, description,
			category, tags, image_url, resolution_date,
			yes_price, no_price, spread,
			volume_24h, open_interest, liquidity,
			status, raw_payload
		FROM markets WHERE status = 'active' AND venue_id = ?
		ORDER BY volume_24h DESC
		LIMIT ?`, venue, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMarkets(rows)
}

// GetMarketsLite returns all active markets for a venue without raw_payload.
// This is much faster and uses less memory than GetAllMarkets -- suitable for
// matching pipelines that only need title, prices, and metadata.
func (s *Store) GetMarketsLite(venue string) ([]*models.CanonicalMarket, error) {
	rows, err := s.db.Query(`
		SELECT venue_id, venue_market_id, venue_event_ticker, venue_series_ticker,
			venue_event_title, venue_slug, title, subtitle, description,
			category, tags, image_url, resolution_date,
			yes_price, no_price, spread,
			volume_24h, open_interest, liquidity,
			status
		FROM markets WHERE status = 'active' AND venue_id = ?
		ORDER BY volume_24h DESC`, venue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMarketsLite(rows)
}

// GetTopMarketsLite returns top N markets by volume without raw_payload.
func (s *Store) GetTopMarketsLite(venue string, limit int) ([]*models.CanonicalMarket, error) {
	rows, err := s.db.Query(`
		SELECT venue_id, venue_market_id, venue_event_ticker, venue_series_ticker,
			venue_event_title, venue_slug, title, subtitle, description,
			category, tags, image_url, resolution_date,
			yes_price, no_price, spread,
			volume_24h, open_interest, liquidity,
			status
		FROM markets WHERE status = 'active' AND venue_id = ?
		ORDER BY volume_24h DESC
		LIMIT ?`, venue, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMarketsLite(rows)
}

// RebuildFTS drops and recreates the FTS5 index from the markets table.
// Useful after schema changes or data imports that bypassed FTS updates.
func (s *Store) RebuildFTS() error {
	fmt.Println("[storage] Rebuilding FTS index...")
	if _, err := s.db.Exec(`DELETE FROM markets_fts`); err != nil {
		return fmt.Errorf("clearing FTS: %w", err)
	}

	rows, err := s.db.Query(`SELECT venue_id, venue_market_id, title, subtitle, description FROM markets`)
	if err != nil {
		return fmt.Errorf("reading markets: %w", err)
	}
	defer rows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO markets_fts (title, subtitle, description, venue_id, venue_market_id) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	count := 0
	for rows.Next() {
		var venueID, marketID, title, subtitle, desc string
		if err := rows.Scan(&venueID, &marketID, &title, &subtitle, &desc); err != nil {
			continue
		}
		stmt.Exec(title, subtitle, desc, venueID, marketID)
		count++
		if count%50000 == 0 {
			fmt.Printf("[storage] FTS indexed %d markets...\n", count)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("[storage] FTS index rebuilt with %d entries\n", count)
	return nil
}
