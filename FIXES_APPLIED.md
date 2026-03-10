# Kalshi API Fixes Applied

## Summary

Your Equinox project had **mismatched struct definitions** that prevented proper parsing of Kalshi API responses. Both the client and normalizer had fields that don't exist in the actual API, and were missing fields that the API does return.

## Changes Made

### 1. Fixed `internal/venues/kalshi/client.go` (lines 220-237)

**Before:**
```go
type kalshiMarket struct {
	// ... other fields ...
	Volume        float64 `json:"volume"`
	OpenInterest  float64 `json:"open_interest"`
	Liquidity     float64 `json:"liquidity"`
	RulesHTML     string  `json:"rules_html"`
	Category      string  `json:"category"`
}
```

**After:**
```go
type kalshiMarket struct {
	// ... other fields ...
	Volume        float64 `json:"volume"`
	Volume24h     float64 `json:"volume_24h"`      // ← ADDED
	OpenInterest  float64 `json:"open_interest"`
	Liquidity     float64 `json:"liquidity"`
	RulesPrimary  string  `json:"rules_primary"`  // ← CHANGED from rules_html
	// ✓ REMOVED: Category (doesn't exist in API)
	// ✓ REMOVED: RulesHTML (doesn't exist in API)
}
```

**What changed:**
- ✅ Added `Volume24h` field (API has `volume_24h`)
- ✅ Changed `RulesHTML` → `RulesPrimary` (API has `rules_primary`, not `rules_html`)
- ✅ Removed `Category` (Kalshi API doesn't return this field)

---

### 2. Fixed `internal/normalizer/normalizer.go` (lines 179-196)

**Before:**
```go
type kalshiRaw struct {
	Ticker       string  `json:"ticker"`
	// ... other fields ...
	Liquidity    float64 `json:"liquidity"`
	RulesPrimary string  `json:"rules_primary"`
	RulesHTML    string  `json:"rules_html"`
	Category     string  `json:"category"`
}
```

**After:**
```go
type kalshiRaw struct {
	Ticker        string  `json:"ticker"`
	EventTicker   string  `json:"event_ticker"`
	Title         string  `json:"title"`
	Subtitle      string  `json:"subtitle"`
	Status        string  `json:"status"`         // ← ADDED
	// ... other fields ...
	RulesPrimary  string  `json:"rules_primary"`
	RulesSecondary string `json:"rules_secondary"` // ← ADDED
	// ✓ REMOVED: RulesHTML (doesn't exist in API)
	// ✓ REMOVED: Category (doesn't exist in API)
}
```

**What changed:**
- ✅ Added `Status` field (needed for status filtering)
- ✅ Added `RulesSecondary` field (API has `rules_secondary`)
- ✅ Removed `RulesHTML` (doesn't exist)
- ✅ Removed `Category` (doesn't exist)

---

### 3. Fixed Kalshi normalization logic (line 217)

**Before:**
```go
Category: models.NormalizeCategory(strings.ToLower(raw.Category)),
```

**After:**
```go
Category: "other",
```

**Why:** The `raw.Category` field no longer exists (Kalshi API doesn't return it), so we default to "other".

---

### 4. Fixed description fallback (lines 242-246)

**Before:**
```go
if m.Description == "" && raw.RulesPrimary != "" {
	m.Description = raw.RulesPrimary
} else if m.Description == "" && raw.RulesHTML != "" {
	m.Description = stripHTMLTags(raw.RulesHTML)
}
```

**After:**
```go
if m.Description == "" && raw.RulesPrimary != "" {
	m.Description = raw.RulesPrimary
} else if m.Description == "" && raw.RulesSecondary != "" {
	m.Description = raw.RulesSecondary
}
```

**Why:** The API has `rules_secondary`, not `rules_html`. Also removed the `stripHTMLTags()` call since `rules_secondary` is plain text, not HTML.

---

## Testing

### Test 1: Verify API is working
```bash
curl "https://api.elections.kalshi.com/trade-api/v2/markets?status=open&limit=1" \
  | jq '.markets[0] | {ticker, title, yes_bid, yes_ask, status, volume_24h, liquidity}'
```

Expected output: Should show market with all those fields populated.

### Test 2: Run Equinox to verify parsing
```bash
# Without API key (public data only)
go run ./cmd/equinox -mode=match -output=json

# With API key (if you have one)
KALSHI_API_KEY=your_key go run ./cmd/equinox -mode=match -output=json
```

Expected output: Should show normalized markets from Kalshi without errors.

---

## Why This Happened

Your code defined struct fields based on **assumptions** about the Kalshi API rather than the actual API responses. The two struct definitions (in client.go and normalizer.go) diverged over time:

- The client had some fields the normalizer didn't have
- The normalizer had some fields that didn't exist in the API
- Both had the non-existent `Category` and `RulesHTML` fields

This is a common issue in Go when working with external APIs. The best practice is to:
1. **Test against the actual API** to verify field names
2. **Use `omitempty` and pointers** for optional fields
3. **Validate unmarshalling errors** to catch mismatches early

---

## Files Modified

1. `internal/venues/kalshi/client.go` - Fixed struct definition
2. `internal/normalizer/normalizer.go` - Fixed struct definition + parsing logic

## What Should Work Now

- ✅ Markets fetch from Kalshi without parsing errors
- ✅ All prices, liquidity, volume parsed correctly
- ✅ Status filtering works (active markets only)
- ✅ Volume24h available for matching
- ✅ Rules/description enrichment works

## Root Cause Analysis

See `KALSHI_API_MISMATCH.md` for the detailed analysis of why this happened and how the external repo handles this more robustly with lenient parsing.
