# Kalshi API Response Mismatch — Diagnosis

## The Problem

Your code has **two different JSON struct definitions** for the same Kalshi market response, and **both are missing fields** that the API actually returns. The API is working, but your code isn't parsing it correctly.

---

## Struct Mismatch

### Issue 1: `internal/venues/kalshi/client.go` (lines 221-237)

```go
type kalshiMarket struct {
	Ticker        string  `json:"ticker"`
	EventTicker   string  `json:"event_ticker"`
	Title         string  `json:"title"`
	Subtitle      string  `json:"subtitle"`
	Category      string  `json:"category"`        // ❌ NOT IN API
	Status        string  `json:"status"`          // ✅ Good, needed
	CloseTime     string  `json:"close_time"`
	YesBid        int     `json:"yes_bid"`
	YesAsk        int     `json:"yes_ask"`
	NoBid         int     `json:"no_bid"`
	NoAsk         int     `json:"no_ask"`
	Volume        float64 `json:"volume"`
	OpenInterest  float64 `json:"open_interest"`
	Liquidity     float64 `json:"liquidity"`
	RulesHTML     string  `json:"rules_html"`      // ❌ NOT IN API
}
```

**Missing:**
- `Volume24h` (API has: `volume_24h`)
- `RulesPrimary` (API has: `rules_primary`)

---

### Issue 2: `internal/normalizer/normalizer.go` (lines 179-196)

```go
type kalshiRaw struct {
	Ticker       string  `json:"ticker"`
	EventTicker  string  `json:"event_ticker"`
	Title        string  `json:"title"`
	Subtitle     string  `json:"subtitle"`
	Category     string  `json:"category"`         // ❌ NOT IN API
	CloseTime    string  `json:"close_time"`
	YesBid       int     `json:"yes_bid"`
	YesAsk       int     `json:"yes_ask"`
	NoBid        int     `json:"no_bid"`
	NoAsk        int     `json:"no_ask"`
	Volume       float64 `json:"volume"`
	Volume24h    float64 `json:"volume_24h"`      // ✅ Good, API has this
	OpenInterest float64 `json:"open_interest"`
	Liquidity    float64 `json:"liquidity"`
	RulesPrimary string  `json:"rules_primary"`   // ✅ Good, API has this
	RulesHTML    string  `json:"rules_html"`      // ❌ NOT IN API
}
```

**Missing:**
- `Status` (API has: `status`)

---

## What the ACTUAL Kalshi API Returns

Test response from: `https://api.elections.kalshi.com/trade-api/v2/markets?status=open&limit=1`

**Available fields:**
```
ticker                  ✅ (string)
event_ticker            ✅ (string)
title                   ✅ (string)
subtitle                ✅ (string)
status                  ✅ (string) — "active", etc.
close_time              ✅ (string) — RFC3339 format
yes_bid                 ✅ (int) — 0-100 cents
yes_ask                 ✅ (int) — 0-100 cents
no_bid                  ✅ (int) — 0-100 cents
no_ask                  ✅ (int) — 0-100 cents
volume                  ✅ (float64)
volume_24h              ✅ (float64)
open_interest           ✅ (float64)
liquidity               ✅ (float64)
rules_primary           ✅ (string)
rules_secondary         ✅ (string)

category                ❌ NOT RETURNED
rules_html              ❌ NOT RETURNED
```

---

## The Fix

### Step 1: Fix `kalshiMarket` struct (client.go)

Replace lines 221-237 with:

```go
type kalshiMarket struct {
	Ticker        string  `json:"ticker"`
	EventTicker   string  `json:"event_ticker"`
	Title         string  `json:"title"`
	Subtitle      string  `json:"subtitle"`
	Status        string  `json:"status"`
	CloseTime     string  `json:"close_time"`
	YesBid        int     `json:"yes_bid"`
	YesAsk        int     `json:"yes_ask"`
	NoBid         int     `json:"no_bid"`
	NoAsk         int     `json:"no_ask"`
	Volume        float64 `json:"volume"`
	Volume24h     float64 `json:"volume_24h"`       // ADD THIS
	OpenInterest  float64 `json:"open_interest"`
	Liquidity     float64 `json:"liquidity"`
	RulesPrimary  string  `json:"rules_primary"`   // CHANGE FROM rules_html
	RulesHTML     string  `json:"rules_secondary"` // OPTIONAL: keep for secondary rules
	// REMOVE: Category (doesn't exist in API)
}
```

### Step 2: Fix `kalshiRaw` struct (normalizer.go)

Replace lines 179-196 with:

```go
type kalshiRaw struct {
	Ticker       string  `json:"ticker"`
	EventTicker  string  `json:"event_ticker"`
	Title        string  `json:"title"`
	Subtitle     string  `json:"subtitle"`
	Status       string  `json:"status"`           // ADD THIS
	CloseTime    string  `json:"close_time"`
	YesBid       int     `json:"yes_bid"`
	YesAsk       int     `json:"yes_ask"`
	NoBid        int     `json:"no_bid"`
	NoAsk        int     `json:"no_ask"`
	Volume       float64 `json:"volume"`
	Volume24h    float64 `json:"volume_24h"`
	OpenInterest float64 `json:"open_interest"`
	Liquidity    float64 `json:"liquidity"`
	RulesPrimary string  `json:"rules_primary"`
	RulesSecondary string `json:"rules_secondary"` // ADD THIS (optional)
	// REMOVE: Category (doesn't exist in API)
	// REMOVE: RulesHTML (doesn't exist in API)
}
```

### Step 3: Update status filter in client.go

Line 394 in `client.go` already checks:
```go
if m.Status != "active" {
    continue
}
```

This is correct and matches the API response. The `kalshiMarket.Status` field needs to exist (which the fix adds).

### Step 4: Remove Category handling in normalizer.go

Line 217 currently does:
```go
Category: models.NormalizeCategory(strings.ToLower(raw.Category)),
```

Since `raw.Category` will always be empty, change to:
```go
Category: models.CategoryUnknown,  // or empty string, or derive from title
```

---

## Testing the Fix

After applying the changes:

```bash
# Test the raw API response
curl "https://api.elections.kalshi.com/trade-api/v2/markets?status=open&limit=1" | jq '.markets[0] | {ticker, title, yes_bid, yes_ask, status}'

# Run Equinox
KALSHI_API_KEY=your_api_key go run ./cmd/equinox -mode=match -output=json

# Or without API key (public data only)
go run ./cmd/equinox -mode=match -output=json
```

---

## Why This Happened

The code was written based on **assumptions about the Kalshi API shape** rather than from actual API responses. The two struct definitions diverged over time as different developers worked on different layers.

Compare with external repo's `Market` model — they use `from_dict()` and handle optional fields gracefully with defaults.

---

## Summary of Changes Needed

| File | Lines | Change | Reason |
|------|-------|--------|--------|
| `client.go` | 236 | Add `Volume24h` field | Normalizer needs this |
| `client.go` | 236 | Change `RulesHTML` to `RulesPrimary` | API has `rules_primary`, not `rules_html` |
| `client.go` | 226 | Remove `Category` | Not in API response |
| `normalizer.go` | 184 | Remove `Category` | Not in API response |
| `normalizer.go` | 195 | Remove `RulesHTML` | API has `rules_primary` + `rules_secondary` |
| `normalizer.go` | - | Add `Status` | Needed for status filtering |
| `normalizer.go` | 217 | Update Category logic | Can't use `raw.Category` anymore |

---

## Root Cause: What External Repo Does Better

The Python external repo uses **lenient parsing** with `from_dict()` factory methods:

```python
def from_dict(cls, data):
    market = cls()
    market.ticker = data.get('ticker', '')
    market.volume_24h = data.get('volume_24h', 0.0)
    # ... etc
    return market
```

This gracefully handles:
- Missing fields (defaults to empty/zero)
- Extra fields (ignored automatically)
- Type mismatches (with defaults)

Your Go code expects exact struct matches, which breaks when the API schema doesn't match your struct definitions.
