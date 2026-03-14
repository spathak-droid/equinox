package kalshi

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/equinox/internal/models"
	"github.com/equinox/internal/venues"
)

// flattenHits converts v1 search series hits into flat RawMarket slices.
// Each market gets series/event context injected so the normalizer can build
// a full CanonicalMarket.
func flattenHits(hits []seriesHit) []*venues.RawMarket {
	var out []*venues.RawMarket
	seen := map[string]struct{}{}

	const maxMarketsPerSeries = 10
	for _, hit := range hits {
		hitCount := 0
		for _, mkt := range hit.Markets {
			if _, ok := seen[mkt.Ticker]; ok {
				continue
			}
			// Skip resolved markets
			if mkt.Result == "yes" || mkt.Result == "no" {
				continue
			}
			if hitCount >= maxMarketsPerSeries {
				break
			}
			seen[mkt.Ticker] = struct{}{}

			// Build a payload the normalizer's kalshiRaw struct can parse.
			// Prefer the series-level custom image (meaningful thumbnail).
			// Fall back to the market-level icon only if custom is absent.
			imageURL := hit.ProductMetadata.CustomImageURL
			if imageURL == "" {
				imageURL = mkt.IconURLLightMode
			}

			payload := map[string]interface{}{
				"ticker":               mkt.Ticker,
				"event_ticker":         hit.EventTicker,
				"series_ticker":        hit.SeriesTicker,
				"event_title":          hit.EventTitle,
				"title":                hit.EventTitle,
				"subtitle":             mkt.YesSubtitle,
				"status":               "active",
				"close_time":           mkt.CloseTS,
				"yes_bid":              mkt.YesBid,
				"yes_ask":              mkt.YesAsk,
				"no_bid":               100 - mkt.YesAsk,
				"no_ask":               100 - mkt.YesBid,
				"volume":               mkt.Volume,
				"volume_24h":           0,
				"open_interest":        0,
				"liquidity":            0,
				"image_url_light_mode": imageURL,
				"image_url_dark_mode":  mkt.IconURLDarkMode,
			}

			b, err := json.Marshal(payload)
			if err != nil {
				continue
			}

			out = append(out, &venues.RawMarket{
				VenueID:       models.VenueKalshi,
				VenueMarketID: mkt.Ticker,
				FetchCategory: strings.ToLower(hit.Category),
				Payload:       b,
			})
			hitCount++
		}
	}
	return out
}

// dollarsToCents converts a dollar string like "0.4200" to integer cents (42).
func dollarsToCents(s string) int {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(math.Round(f * 100))
}

// parseFloatStr parses a numeric string, returning 0 on failure.
func parseFloatStr(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
