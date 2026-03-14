package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/equinox/internal/models"
)

// scanMarkets reads rows into CanonicalMarket slices.
func scanMarkets(rows *sql.Rows) ([]*models.CanonicalMarket, error) {
	var markets []*models.CanonicalMarket
	for rows.Next() {
		var venueID, venueMarketID, eventTicker, seriesTicker string
		var eventTitle, slug, title, subtitle, description string
		var category, tagsStr, imageURL string
		var resDateStr sql.NullString
		var yesPrice, noPrice, spread float64
		var vol24h, oi, liq float64
		var status string
		var rawPayload sql.NullString

		err := rows.Scan(
			&venueID, &venueMarketID, &eventTicker, &seriesTicker,
			&eventTitle, &slug, &title, &subtitle, &description,
			&category, &tagsStr, &imageURL, &resDateStr,
			&yesPrice, &noPrice, &spread,
			&vol24h, &oi, &liq,
			&status, &rawPayload,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning market row: %w", err)
		}

		m := buildCanonicalMarket(
			venueID, venueMarketID, eventTicker, seriesTicker,
			eventTitle, slug, title, subtitle, description,
			category, tagsStr, imageURL, resDateStr,
			yesPrice, noPrice, spread,
			vol24h, oi, liq,
			status,
		)

		if rawPayload.Valid {
			m.RawPayload = json.RawMessage(rawPayload.String)
		}

		markets = append(markets, m)
	}
	return markets, rows.Err()
}

// scanMarketsLite reads rows into CanonicalMarket slices without raw_payload.
func scanMarketsLite(rows *sql.Rows) ([]*models.CanonicalMarket, error) {
	var markets []*models.CanonicalMarket
	for rows.Next() {
		var venueID, venueMarketID, eventTicker, seriesTicker string
		var eventTitle, slug, title, subtitle, description string
		var category, tagsStr, imageURL string
		var resDateStr sql.NullString
		var yesPrice, noPrice, spread float64
		var vol24h, oi, liq float64
		var status string

		err := rows.Scan(
			&venueID, &venueMarketID, &eventTicker, &seriesTicker,
			&eventTitle, &slug, &title, &subtitle, &description,
			&category, &tagsStr, &imageURL, &resDateStr,
			&yesPrice, &noPrice, &spread,
			&vol24h, &oi, &liq,
			&status,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning market row: %w", err)
		}

		m := buildCanonicalMarket(
			venueID, venueMarketID, eventTicker, seriesTicker,
			eventTitle, slug, title, subtitle, description,
			category, tagsStr, imageURL, resDateStr,
			yesPrice, noPrice, spread,
			vol24h, oi, liq,
			status,
		)

		markets = append(markets, m)
	}
	return markets, rows.Err()
}

// buildCanonicalMarket constructs a CanonicalMarket from scanned field values.
func buildCanonicalMarket(
	venueID, venueMarketID, eventTicker, seriesTicker string,
	eventTitle, slug, title, subtitle, description string,
	category, tagsStr, imageURL string,
	resDateStr sql.NullString,
	yesPrice, noPrice, spread float64,
	vol24h, oi, liq float64,
	status string,
) *models.CanonicalMarket {
	m := &models.CanonicalMarket{
		VenueID:           models.VenueID(venueID),
		VenueMarketID:     venueMarketID,
		VenueEventTicker:  eventTicker,
		VenueSeriesTicker: seriesTicker,
		VenueEventTitle:   eventTitle,
		VenueSlug:         slug,
		Title:             title,
		Subtitle:          subtitle,
		Description:       description,
		Category:          category,
		ImageURL:          imageURL,
		YesPrice:          yesPrice,
		NoPrice:           noPrice,
		Spread:            spread,
		Volume24h:         vol24h,
		OpenInterest:      oi,
		Liquidity:         liq,
		Status:            models.MarketStatus(status),
	}

	if tagsStr != "" {
		m.Tags = strings.Split(tagsStr, ",")
	}
	if resDateStr.Valid && resDateStr.String != "" {
		t, err := time.Parse(time.RFC3339, resDateStr.String)
		if err == nil {
			m.ResolutionDate = &t
		}
	}

	return m
}

// ftsStopwords are common words stripped from OR queries to reduce noise.
var ftsStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true, "was": true,
	"will": true, "be": true, "to": true, "of": true, "in": true, "for": true,
	"and": true, "or": true, "on": true, "at": true, "by": true, "with": true,
	"this": true, "that": true, "it": true, "its": true, "from": true, "has": true,
	"have": true, "had": true, "do": true, "does": true, "did": true,
	"not": true, "but": true, "if": true, "than": true,
}

// sanitizeFTSQuery cleans a query string for FTS5 compatibility.
// Words are joined with implicit AND (FTS5 default).
func sanitizeFTSQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}

	q = ftsReplacer.Replace(q)

	words := strings.Fields(q)
	if len(words) == 0 {
		return ""
	}

	// Join with implicit AND (FTS5 default)
	return strings.Join(words, " ")
}

// sanitizeFTSQueryOR cleans query and joins words with OR for fuzzy matching.
func sanitizeFTSQueryOR(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}

	q = ftsReplacer.Replace(q)

	words := strings.Fields(q)
	var filtered []string
	for _, w := range words {
		w = strings.ToLower(w)
		if len(w) < 2 || ftsStopwords[w] {
			continue
		}
		filtered = append(filtered, w)
	}
	if len(filtered) == 0 {
		return ""
	}

	return strings.Join(filtered, " OR ")
}

// ftsReplacer removes FTS5 special characters that could cause parse errors.
var ftsReplacer = strings.NewReplacer(
	"\"", "", "'", "", "*", "", "(", "", ")", "",
	":", "", "^", "", "~", "", "{", "", "}", "",
	"[", "", "]", "", "+", "", "-", " ", "!", "",
	"@", "", "#", "", "$", "", "%", "", "&", "",
	"|", "", "\\", "", "/", " ", "?", "", ".", " ",
	",", " ", ";", " ",
)
