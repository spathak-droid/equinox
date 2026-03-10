# External Repo Analysis: Prediction Market Analysis Framework

**Repository**: https://github.com/Jon-Becker/prediction-market-analysis
**Language**: Python
**Purpose**: Historical data collection and analysis for prediction markets

---

## Alignment Assessment

### ❌ Not Aligned for Direct Integration

The external repo and Project Equinox are fundamentally **different architectures**:

| Aspect | External Repo | Equinox |
|--------|---------------|---------|
| **Language** | Python | Go |
| **Primary Goal** | Historical data analysis & statistical research | Real-time market matching & routing |
| **Data Flow** | Collect → Persist (Parquet) → Analyze | Fetch → Normalize → Match → Route (streaming) |
| **Persistence** | Heavy (Parquet files + indexed datasets) | None (ephemeral, no database) |
| **Use Case** | Retrospective market research | Live trading decision support |
| **API Approach** | Event-driven indexers with progress tracking | Simple client interface with immediate processing |

### Key Differences

#### 1. **Data Scope**
- **External**: Fetches historical trades, blockchain events, detailed OHLCV data
- **Equinox**: Fetches current market snapshots only (prices, liquidity, open interest)

#### 2. **Architecture Paradigm**
- **External**: Multi-layer pipeline (Indexers → Parquet → Analysis Scripts)
- **Equinox**: Single-pass linear pipeline (Fetch → Normalize → Match → Route)

#### 3. **Market Coverage**
Both support **Kalshi + Polymarket**, but with different depths:
- **External**: Kalshi public markets + full trade history + blockchain events for Polymarket
- **Equinox**: Active markets only, prices + liquidity data, no trade history

---

## What We Could Learn (Not Integrate)

### 1. **Kalshi API Best Practices** ✓
The external repo demonstrates:
- Series ticker vs. event ticker distinction for searching
- Cursor-based pagination patterns
- Optional authentication for enhanced data access
- Field names and structure (confirms Equinox implementation is correct)

**Status**: Equinox already implements all of these correctly.

### 2. **Data Normalization Strategy** ✓
External repo normalizes venues via `factory methods` (`from_dict()`):
```python
# External approach
market = Market.from_dict(api_response)
trade = Trade.from_dict(api_response)
```

**Equinox equivalent**: `normalizer.go` with venue-specific JSON unmarshalling (same pattern, different language).

### 3. **Error Handling & Resilience**
- Retry logic with exponential backoff (external has `@retry_request()` decorator)
- Graceful degradation when APIs are unavailable
- Progress tracking to resume interrupted runs

**Status**: Equinox has basic retry logic in `kalshi/client.go` lines 348-369. Could be enhanced.

---

## Kalshi Integration Status in Equinox

### ✅ Already Implemented

#### Kalshi Client (`internal/venues/kalshi/client.go`)
- ✅ API base URL: `https://api.elections.kalshi.com/trade-api/v2`
- ✅ Endpoints: `/markets`, `/markets?series_ticker=X`, `/markets?event_ticker=X`, `/events`
- ✅ Authentication: Bearer token support (line 272)
- ✅ Pagination: Cursor-based with configurable page size (lines 102-137)
- ✅ Series/Event search: V1 search API integration (lines 277-344)
- ✅ Event title caching: Prevents repeated lookups (lines 445-494)
- ✅ Rate limit handling: 429 backoff (lines 464-470)
- ✅ Market filtering: Status checks, synthetic market filtering (lines 397-399, 644-653)

#### Normalizer (`internal/normalizer/normalizer.go` lines 177-249)
- ✅ Price conversion: Cents → [0.0, 1.0] (line 205-206)
- ✅ Spread calculation: YES ask-bid in normalized units (line 208)
- ✅ Metadata parsing: Ticker, title, subtitle, category
- ✅ Resolution date: RFC3339 parsing (line 232)
- ✅ Description enrichment: Falls back to rules if subtitle empty (lines 242-246)

#### Main Wiring (`cmd/equinox/main.go`)
- ✅ Venue client instantiation (line 93)
- ✅ Market fetching (line 108)
- ✅ Normalization pipeline (line 119)
- ✅ Config integration (line 82)

### 📋 What's NOT in the External Repo (Equinox is Ahead)

1. **Equivalence Matching** - Hybrid fuzzy + embedding detection (not in external repo)
2. **Routing Simulation** - Venue selection logic with configurable weights (not in external repo)
3. **AI Embeddings** - Optional OpenAI enrichment (not in external repo)
4. **Real-time Decision Logs** - Human-readable routing explanations (not in external repo)

---

## Recommendations

### 1. **DO NOT INTEGRATE** the external repo as-is
- Different language (Python vs Go)
- Different architecture (batch processing vs real-time)
- Different persistence model (requires database/Parquet)
- Would require rewriting in Go

### 2. **Use External Repo FOR REFERENCE ONLY**
If you need additional features from the external repo, reimplements them in Go:
- **Trade history indexing**: Would require new `trade/` routes in both clients + schema in canonical.go
- **Blockchain event tracking**: Polymarket-specific, requires new schema
- **Statistical analysis**: Build on top of existing routing pipeline
- **Dashboard/visualization**: Would require UI layer (explicitly out of scope per CLAUDE.md)

### 3. **Kalshi Enhancement Opportunities** (aligned with Equinox)
Currently implemented but could be expanded:

**A. Add trade history support**
```go
// New method in kalshi/client.go
func (c *Client) FetchTrades(ctx context.Context, ticker string) ([]*Trade, error)

// New struct in canonical.go for trade data
type TradeRecord struct {
    ID string
    Price float64
    Quantity int
    Timestamp time.Time
}
```

**B. Enhance liquidity detection**
Current: Uses `liquidity` field from API (already captured in canonical.go)
Enhancement: Calculate from bid-ask volumes if available

**C. Add venue-specific market features**
Example: Kalshi's "milestones" or "series categories"
→ Add optional fields to `CanonicalMarket` without breaking existing code

### 4. **Testing Recommendations**
Add unit tests using external repo's API response shapes:
```bash
curl "https://api.elections.kalshi.com/trade-api/v2/markets?status=open&limit=1" | jq . > testdata/kalshi_market.json
curl "https://api.elections.kalshi.com/trade-api/v2/events?limit=1" | jq . > testdata/kalshi_event.json
```

Then add test cases in `normalizer_test.go` and `kalshi/client_test.go`.

---

## Conclusion

**External repo alignment: ~40%** (similar venues, totally different approach)

**Kalshi in Equinox: COMPLETE** ✅

The external repo confirms Equinox's Kalshi implementation is correct. Rather than integrating it, focus on:
1. **Adding trade history** if routing needs execution context
2. **Adding tests** to validate against live API responses
3. **Performance profiling** if matching becomes a bottleneck
4. **Third venue** (Manifold Markets) to demonstrate extensibility per CLAUDE.md

No changes to Kalshi integration are required based on external repo analysis.
