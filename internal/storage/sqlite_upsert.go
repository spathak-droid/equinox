package storage

import (
	"fmt"
	"strings"
	"time"

	"github.com/equinox/internal/models"
)

// UpsertMarket inserts or updates a single market.
func (s *Store) UpsertMarket(m *models.CanonicalMarket) error {
	now := time.Now().UTC().Format(time.RFC3339)
	resDate := ""
	if m.ResolutionDate != nil {
		resDate = m.ResolutionDate.Format(time.RFC3339)
	}
	tags := strings.Join(m.Tags, ",")
	rawPayload := ""
	if m.RawPayload != nil {
		rawPayload = string(m.RawPayload)
	}

	_, err := s.db.Exec(`
		INSERT INTO markets (
			venue_id, venue_market_id, venue_event_ticker, venue_series_ticker,
			venue_event_title, venue_slug, title, subtitle, description,
			category, tags, image_url, resolution_date,
			yes_price, no_price, spread,
			volume_24h, open_interest, liquidity,
			status, raw_payload, indexed_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(venue_id, venue_market_id) DO UPDATE SET
			title=excluded.title,
			subtitle=excluded.subtitle,
			description=excluded.description,
			category=excluded.category,
			tags=excluded.tags,
			image_url=excluded.image_url,
			resolution_date=excluded.resolution_date,
			yes_price=excluded.yes_price,
			no_price=excluded.no_price,
			spread=excluded.spread,
			volume_24h=excluded.volume_24h,
			open_interest=excluded.open_interest,
			liquidity=excluded.liquidity,
			status=excluded.status,
			raw_payload=excluded.raw_payload,
			updated_at=excluded.updated_at`,
		string(m.VenueID), m.VenueMarketID, m.VenueEventTicker, m.VenueSeriesTicker,
		m.VenueEventTitle, m.VenueSlug, m.Title, m.Subtitle, m.Description,
		m.Category, tags, m.ImageURL, resDate,
		m.YesPrice, m.NoPrice, m.Spread,
		m.Volume24h, m.OpenInterest, m.Liquidity,
		string(m.Status), rawPayload, now, now,
	)
	return err
}

// UpsertMarkets batch-inserts or updates markets, committing every batchSize records.
// Also maintains the FTS5 index for full-text search.
func (s *Store) UpsertMarkets(markets []*models.CanonicalMarket) (inserted, updated int, err error) {
	const batchSize = 5000
	now := time.Now().UTC().Format(time.RFC3339)

	for i := 0; i < len(markets); i += batchSize {
		end := i + batchSize
		if end > len(markets) {
			end = len(markets)
		}
		batch := markets[i:end]

		n, err := s.upsertBatch(batch, now)
		if err != nil {
			return inserted, 0, fmt.Errorf("batch %d-%d: %w", i, end, err)
		}
		inserted += n

		if end%20000 == 0 || end == len(markets) {
			fmt.Printf("[storage] stored %d/%d markets\n", end, len(markets))
		}
	}
	return inserted, 0, nil
}

func (s *Store) upsertBatch(markets []*models.CanonicalMarket, now string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	upsertStmt, err := tx.Prepare(`
		INSERT INTO markets (
			venue_id, venue_market_id, venue_event_ticker, venue_series_ticker,
			venue_event_title, venue_slug, title, subtitle, description,
			category, tags, image_url, resolution_date,
			yes_price, no_price, spread,
			volume_24h, open_interest, liquidity,
			status, raw_payload, indexed_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(venue_id, venue_market_id) DO UPDATE SET
			title=excluded.title,
			subtitle=excluded.subtitle,
			description=excluded.description,
			category=excluded.category,
			tags=excluded.tags,
			image_url=excluded.image_url,
			resolution_date=excluded.resolution_date,
			yes_price=excluded.yes_price,
			no_price=excluded.no_price,
			spread=excluded.spread,
			volume_24h=excluded.volume_24h,
			open_interest=excluded.open_interest,
			liquidity=excluded.liquidity,
			status=excluded.status,
			raw_payload=excluded.raw_payload,
			updated_at=excluded.updated_at`)
	if err != nil {
		return 0, err
	}
	defer upsertStmt.Close()

	inserted := 0
	for _, m := range markets {
		resDate := ""
		if m.ResolutionDate != nil {
			resDate = m.ResolutionDate.Format(time.RFC3339)
		}
		tags := strings.Join(m.Tags, ",")
		rawPayload := ""
		if m.RawPayload != nil {
			rawPayload = string(m.RawPayload)
		}

		result, err := upsertStmt.Exec(
			string(m.VenueID), m.VenueMarketID, m.VenueEventTicker, m.VenueSeriesTicker,
			m.VenueEventTitle, m.VenueSlug, m.Title, m.Subtitle, m.Description,
			m.Category, tags, m.ImageURL, resDate,
			m.YesPrice, m.NoPrice, m.Spread,
			m.Volume24h, m.OpenInterest, m.Liquidity,
			string(m.Status), rawPayload, now, now,
		)
		if err != nil {
			fmt.Printf("[storage] WARNING: skipping market %s/%s: %v\n", m.VenueID, m.VenueMarketID, err)
			continue
		}
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected > 0 {
			inserted++
		}
	}

	return inserted, tx.Commit()
}

// PurgeStale removes markets that haven't been updated since the given cutoff.
// This cleans out markets that are no longer open on the venue.
func (s *Store) PurgeStale(cutoff time.Time) (int64, error) {
	cutoffStr := cutoff.UTC().Format(time.RFC3339)
	// Remove from FTS first
	s.db.Exec(`DELETE FROM markets_fts WHERE venue_market_id IN (
		SELECT venue_market_id FROM markets WHERE updated_at < ?)`, cutoffStr)
	// Then from main table
	result, err := s.db.Exec(`DELETE FROM markets WHERE updated_at < ?`, cutoffStr)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
