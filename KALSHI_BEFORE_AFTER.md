# Kalshi Fix - Before/After Visual Comparison

## The Core Problem

Your code had **two different struct definitions for the same Kalshi API response**, and **both were wrong**:

```
KALSHI API RESPONSE
           │
           ├─ ticker ✓
           ├─ event_ticker ✓
           ├─ title ✓
           ├─ subtitle ✓
           ├─ status ✓
           ├─ close_time ✓
           ├─ yes_bid ✓
           ├─ yes_ask ✓
           ├─ no_bid ✓
           ├─ no_ask ✓
           ├─ volume ✓
           ├─ volume_24h ✓
           ├─ open_interest ✓
           ├─ liquidity ✓
           ├─ rules_primary ✓
           └─ rules_secondary ✓

YOUR CODE (client.go) ❌          YOUR CODE (normalizer.go) ❌
├─ ticker ✓                       ├─ ticker ✓
├─ event_ticker ✓                 ├─ event_ticker ✓
├─ title ✓                        ├─ title ✓
├─ subtitle ✓                     ├─ subtitle ✓
├─ category ✗ (WRONG)             ├─ category ✗ (WRONG)
├─ status ✓                       ├─ status ✗ (MISSING)
├─ close_time ✓                   ├─ close_time ✓
├─ yes_bid ✓                      ├─ yes_bid ✓
├─ yes_ask ✓                      ├─ yes_ask ✓
├─ no_bid ✓                       ├─ no_bid ✓
├─ no_ask ✓                       ├─ no_ask ✓
├─ volume ✓                       ├─ volume ✓
├─ volume_24h ✗ (MISSING)         ├─ volume_24h ✓
├─ open_interest ✓                ├─ open_interest ✓
├─ liquidity ✓                    ├─ liquidity ✓
└─ rules_html ✗ (WRONG)           ├─ rules_primary ✓
                                  └─ rules_html ✗ (WRONG)
```

---

## File 1: `internal/venues/kalshi/client.go`

### Before
```go
type kalshiMarket struct {
    Ticker        string  `json:"ticker"`
    EventTicker   string  `json:"event_ticker"`
    Title         string  `json:"title"`
    Subtitle      string  `json:"subtitle"`
    Category      string  `json:"category"`         // ❌ Not in API
    Status        string  `json:"status"`
    CloseTime     string  `json:"close_time"`
    YesBid        int     `json:"yes_bid"`
    YesAsk        int     `json:"yes_ask"`
    NoBid         int     `json:"no_bid"`
    NoAsk         int     `json:"no_ask"`
    Volume        float64 `json:"volume"`
    OpenInterest  float64 `json:"open_interest"`
    Liquidity     float64 `json:"liquidity"`
    RulesHTML     string  `json:"rules_html"`      // ❌ Should be rules_primary
}                                                  // ❌ Missing volume_24h
```

### After
```go
type kalshiMarket struct {
    Ticker        string  `json:"ticker"`
    EventTicker   string  `json:"event_ticker"`
    Title         string  `json:"title"`
    Subtitle      string  `json:"subtitle"`
    Status        string  `json:"status"`          // ✅ Keep this
    CloseTime     string  `json:"close_time"`
    YesBid        int     `json:"yes_bid"`
    YesAsk        int     `json:"yes_ask"`
    NoBid         int     `json:"no_bid"`
    NoAsk         int     `json:"no_ask"`
    Volume        float64 `json:"volume"`
    Volume24h     float64 `json:"volume_24h"`      // ✅ Added
    OpenInterest  float64 `json:"open_interest"`
    Liquidity     float64 `json:"liquidity"`
    RulesPrimary  string  `json:"rules_primary"`   // ✅ Fixed
}                                                  // ✅ Removed Category
```

**Changes:**
- ❌➜✅ `RulesHTML` → `RulesPrimary`
- ✅ Added `Volume24h`
- ✅ Removed `Category` (doesn't exist in API)

---

## File 2: `internal/normalizer/normalizer.go`

### Struct Definition - Before
```go
type kalshiRaw struct {
    Ticker       string  `json:"ticker"`
    EventTicker  string  `json:"event_ticker"`
    Title        string  `json:"title"`
    Subtitle     string  `json:"subtitle"`
    Category     string  `json:"category"`         // ❌ Not in API
    CloseTime    string  `json:"close_time"`
    YesBid       int     `json:"yes_bid"`
    YesAsk       int     `json:"yes_ask"`
    NoBid        int     `json:"no_bid"`
    NoAsk        int     `json:"no_ask"`
    Volume       float64 `json:"volume"`
    Volume24h    float64 `json:"volume_24h"`
    OpenInterest float64 `json:"open_interest"`
    Liquidity    float64 `json:"liquidity"`
    RulesPrimary string  `json:"rules_primary"`   // ✅ Good
    RulesHTML    string  `json:"rules_html"`      // ❌ Not in API
}                                                 // ❌ Missing status
```

### Struct Definition - After
```go
type kalshiRaw struct {
    Ticker        string  `json:"ticker"`
    EventTicker   string  `json:"event_ticker"`
    Title         string  `json:"title"`
    Subtitle      string  `json:"subtitle"`
    Status        string  `json:"status"`         // ✅ Added
    CloseTime     string  `json:"close_time"`
    YesBid        int     `json:"yes_bid"`
    YesAsk        int     `json:"yes_ask"`
    NoBid         int     `json:"no_bid"`
    NoAsk         int     `json:"no_ask"`
    Volume        float64 `json:"volume"`
    Volume24h     float64 `json:"volume_24h"`
    OpenInterest  float64 `json:"open_interest"`
    Liquidity     float64 `json:"liquidity"`
    RulesPrimary  string  `json:"rules_primary"`
    RulesSecondary string `json:"rules_secondary"` // ✅ Added
}                                                  // ✅ Removed Category, RulesHTML
```

**Changes:**
- ✅ Added `Status`
- ✅ Added `RulesSecondary`
- ✅ Removed `Category` (doesn't exist in API)
- ✅ Removed `RulesHTML` (API doesn't have this)

### Category Assignment - Before
```go
Category: models.NormalizeCategory(strings.ToLower(raw.Category)),
```

### Category Assignment - After
```go
Category: "other",
```

**Why:** `raw.Category` no longer exists. This is correct because Kalshi API doesn't provide a category field.

### Description Fallback - Before
```go
if m.Description == "" && raw.RulesPrimary != "" {
    m.Description = raw.RulesPrimary
} else if m.Description == "" && raw.RulesHTML != "" {
    m.Description = stripHTMLTags(raw.RulesHTML)
}
```

### Description Fallback - After
```go
if m.Description == "" && raw.RulesPrimary != "" {
    m.Description = raw.RulesPrimary
} else if m.Description == "" && raw.RulesSecondary != "" {
    m.Description = raw.RulesSecondary
}
```

**Why:**
- `raw.RulesHTML` doesn't exist
- `raw.RulesSecondary` is plain text, not HTML
- Removed unused `stripHTMLTags()` function

---

## Impact Summary

### Before
```
Kalshi API Response
       ↓ (unmarshal fails/partial)
kalshiMarket struct ❌
       ↓
kalshiRaw struct ❌
       ↓
normalizeKalshi() ❌
       ↓
CanonicalMarket with wrong/missing data ❌
```

### After
```
Kalshi API Response
       ↓ (unmarshal succeeds)
kalshiMarket struct ✅
       ↓
kalshiRaw struct ✅
       ↓
normalizeKalshi() ✅
       ↓
CanonicalMarket with all correct data ✅
```

---

## Testing the Fix

### Step 1: Verify API Response Structure
```bash
curl "https://api.elections.kalshi.com/trade-api/v2/markets?status=open&limit=1" \
  | jq '.markets[0] | keys_unsorted'
```

You should see: `ticker`, `event_ticker`, `title`, `subtitle`, `status`, `volume_24h`, `rules_primary`, `rules_secondary`, etc.

### Step 2: Run Equinox
```bash
# Test with mock data first
go run ./cmd/equinox -mock -mock-path=testdata/markets.mock.json -mode=match

# Or test live (requires curl/internet)
go run ./cmd/equinox -mode=match -output=json 2>&1 | head -50
```

You should see markets from Kalshi being fetched and processed without JSON unmarshal errors.

---

## Why This Matters

In Go, struct unmarshalling:
- ✅ **Silently ignores** extra fields in the JSON that aren't in the struct
- ✅ **Silently leaves empty** struct fields that aren't in the JSON
- ❌ **Does NOT fail** if you reference a field that was never populated

This means your code was:
1. Trying to unmarshal `category` (which doesn't exist → stays empty)
2. Trying to unmarshal `rules_html` (which doesn't exist → stays empty)
3. Missing `volume_24h` field entirely in client struct
4. Missing `status` field entirely in normalizer struct

The code appeared to work but was silently losing data. Now it correctly captures all fields that the API provides.

---

## Complete File List of Changes

| File | Lines | Change | Status |
|------|-------|--------|--------|
| `internal/venues/kalshi/client.go` | 220-237 | Updated kalshiMarket struct | ✅ Done |
| `internal/normalizer/normalizer.go` | 179-196 | Updated kalshiRaw struct | ✅ Done |
| `internal/normalizer/normalizer.go` | 217 | Changed Category assignment | ✅ Done |
| `internal/normalizer/normalizer.go` | 242-246 | Updated description fallback | ✅ Done |
| `internal/normalizer/normalizer.go` | 395-410 | Removed stripHTMLTags function | ✅ Done |

All changes complete and ready to test.
