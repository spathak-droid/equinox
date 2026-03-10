# CLAUDE.md — Project Equinox

This file tells Claude Code everything it needs to know to build, run, and extend this project.

---

## What this project is

**Project Equinox** is a Go prototype that:
1. Fetches live markets from two prediction market venues (Polymarket + Kalshi)
2. Normalizes them into a shared internal schema (`CanonicalMarket`)
3. Detects equivalent markets across venues using a hybrid rule + AI embedding pipeline
4. Simulates routing decisions for hypothetical trades, with a human-readable explanation log

This is an **infrastructure prototype** — no real trading, no wallets, no UI.

---

## Commands

```bash
# Install dependencies
go mod tidy

# Run the full pipeline
OPENAI_API_KEY=sk-... go run ./cmd/equinox

# Run without AI (falls back to rule-based matching only)
go run ./cmd/equinox

# Match-only + JSON output for evaluator tooling
go run ./cmd/equinox -mode=match -output=json

# Mock fixture mode for offline deterministic runs
go run ./cmd/equinox -mock -mock-path=testdata/markets.mock.json -mode=match -output=json

# Run with Kalshi auth enabled (more pricing data)
KALSHI_API_KEY=... OPENAI_API_KEY=sk-... go run ./cmd/equinox

# Build binary
go build -o bin/equinox ./cmd/equinox

# Run tests
go test ./...

# Vet
go vet ./...
```

---

## Project structure

```
equinox/
├── cmd/equinox/main.go              Entry point — wires all layers, runs pipeline
├── config/config.go                 Loads config from env vars with defaults
├── go.mod                           Module: github.com/equinox
├── internal/
│   ├── models/canonical.go          CanonicalMarket — the shared internal schema
│   ├── venues/
│   │   ├── venue.go                 Venue interface (all clients implement this)
│   │   ├── polymarket/client.go     Polymarket Gamma API client
│   │   └── kalshi/client.go         Kalshi REST API client
│   ├── normalizer/normalizer.go     Converts raw venue JSON → CanonicalMarket + embeddings
│   ├── matcher/matcher.go           Equivalence detection (4-stage pipeline)
│   └── router/router.go             Venue-agnostic routing engine + decision log
└── README.md                        Architecture writeup (part of the submission)
```

---

## Architecture rules — do not violate these

These constraints are core to the evaluation criteria. The evaluators are specifically checking that layers are cleanly separated.

1. **Venue clients** (`venues/polymarket`, `venues/kalshi`) may only fetch raw JSON. They must not parse or transform data into CanonicalMarket. They return `*venues.RawMarket` only.

2. **Normalizer** is the ONLY package allowed to contain venue-specific field names or parsing logic. It converts `RawMarket → CanonicalMarket`.

3. **Matcher** must not reference any venue-specific field, type, or constant. It operates only on `*models.CanonicalMarket`.

4. **Router** must not reference any venue-specific field, type, or constant. It operates only on `*models.CanonicalMarket` and `*matcher.MatchResult`.

5. **Adding a new venue** should only require: (a) new client in `venues/<name>/`, (b) new normalizer case in `normalizer.go`, (c) registration in `main.go`. The matcher and router must need zero changes.

---

## Key types

### `models.CanonicalMarket` (internal/models/canonical.go)
The shared internal representation. All prices normalized to [0.0, 1.0].
```go
type CanonicalMarket struct {
    ID, VenueID, VenueMarketID string
    Title, Description, Category string
    Tags []string
    ResolutionDate *time.Time     // pointer — may be nil if venue doesn't specify
    YesPrice, NoPrice, Spread float64  // [0.0, 1.0]
    Volume24h, OpenInterest, Liquidity float64  // USD
    Status MarketStatus
    TitleEmbedding []float32      // nil when OPENAI_API_KEY not set
    RawPayload json.RawMessage    // verbatim venue JSON — never read by matcher/router
}
```

### `venues.Venue` interface (internal/venues/venue.go)
```go
type Venue interface {
    ID() models.VenueID
    FetchMarkets(ctx context.Context) ([]*RawMarket, error)
}
```

### `matcher.MatchResult` (internal/matcher/matcher.go)
```go
type MatchResult struct {
    MarketA, MarketB *models.CanonicalMarket
    Confidence       MatchConfidence  // MATCH | PROBABLE_MATCH | NO_MATCH
    CompositeScore   float64
    FuzzyScore       float64
    EmbeddingScore   float64          // -1 if not computed
    Explanation      string
}
```

### `router.RoutingDecision` (internal/router/router.go)
```go
type RoutingDecision struct {
    Order         *Order
    MatchedPair   *matcher.MatchResult
    SelectedVenue *models.CanonicalMarket
    AllScores     []*VenueScore
    FinalScore    float64
    Explanation   string  // human-readable multi-line decision log
}
```

---

## Config (all via environment variables)

| Env var | Default | Purpose |
|---|---|---|
| `OPENAI_API_KEY` | _(empty)_ | Enables embedding matching. Falls back to rules-only if unset. |
| `KALSHI_API_KEY` | _(empty)_ | Kalshi auth. Public markets accessible without it. |
| `MATCH_THRESHOLD` | `0.45` | Composite score to declare two markets equivalent. |
| `PROBABLE_MATCH_THRESHOLD` | `0.35` | Score range held as probable before disambiguation. |
| `MAX_DATE_DELTA_DAYS` | `365` | Max days apart two resolution dates can be to still match. |
| `PRICE_WEIGHT` | `0.60` | Routing weight for price quality. Must sum to 1.0 with others. |
| `LIQUIDITY_WEIGHT` | `0.30` | Routing weight for liquidity. |
| `SPREAD_WEIGHT` | `0.10` | Routing weight for bid-ask spread. |
| `DEFAULT_ORDER_SIZE` | `1000.0` | USD size of hypothetical orders in simulation. |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | OpenAI model for embeddings. |
| `EMBEDDING_CACHE_ENABLED` | `false` | Optional local disk cache for embedding vectors. |
| `EMBEDDING_CACHE_PATH` | `.equinox_embedding_cache.json` | Optional embedding cache location. |
| `HTTP_TIMEOUT_SECONDS` | `30` | Timeout for venue API calls. |

---

## Matching logic (do not change without updating README.md)

Four-stage pipeline in `matcher/matcher.go`:

**Stage 1 — Hard filters** (cheap, applied first):
- Both markets must have `Status == StatusActive`
- If both have resolution dates: `|dateA - dateB| <= MAX_DATE_DELTA_DAYS`

**Stage 2 — Fuzzy title score** (always run):
- `fuzzy = 0.5 × levenshteinSimilarity + 0.5 × keywordJaccard`
- Titles are normalized: lowercase, punctuation stripped, stopwords removed for Jaccard

**Stage 3 — Embedding cosine similarity** (only when `TitleEmbedding != nil`):
- `cosineSimilarity(a.TitleEmbedding, b.TitleEmbedding)`

**Stage 4 — LLM disambiguation** (when OpenAI is configured)
- Ambiguous pairs are sent through a chat model for pairwise `match` / `no_match` / `unsure`
- `match` upgrades to `ConfidenceMatch`
- `no_match` downgrades to `ConfidenceNoMatch`
- `unsure` remains `ConfidenceProbable`

**Composite:**
```
with embeddings:    composite = 0.40 × fuzzy + 0.60 × embedding
without embeddings: composite = fuzzy
```

**Decision:**
```
>= MATCH_THRESHOLD         → ConfidenceMatch
>= PROBABLE_MATCH_THRESHOLD → ConfidenceProbable
else                        → ConfidenceNoMatch
```

---

## Routing logic (do not change without updating README.md)

In `router/router.go`, for each candidate venue:

```
priceScore:
  BUY YES → 1 - yes_price
  BUY NO  → yes_price

liquidityScore:
  tanh(liquidity / order_size)   ← smooth, saturates at 1.0

spreadScore:
  if spread > 0: 1 - min(spread / 0.20, 1.0)
  if spread == 0: 0.5  ← neutral, data not available

routingScore = PriceWeight×price + LiquidityWeight×liquidity + SpreadWeight×spread
```

Selected venue = `argmax(routingScore)`.

---

## Error handling conventions

- **Venue fetch failure**: log warning, skip that venue, continue. Never abort the full run because one venue is down.
- **Malformed market JSON**: log warning with venue+ID, skip the market, continue. Never abort for one bad record.
- **Embedding API failure**: log warning, set all `TitleEmbedding = nil`, fall back to rule-only matching. Non-fatal.
- **Config validation failure**: fatal — exit with error before doing any I/O.

---

## What to build next (priority order)

If asked to extend this project, tackle in this order:

1. **Fix venue API field names** — verify `polymarketRaw` and `kalshiRaw` structs in `normalizer.go` match real API responses. Run:
   ```bash
   curl "https://gamma-api.polymarket.com/markets?limit=1" | jq .
   curl "https://api.elections.kalshi.com/trade-api/v2/markets?status=open&limit=1" | jq .
   ```
   Adjust struct field names and JSON tags accordingly.

2. **Add unit tests** for:
   - `matcher.fuzzyTitleScore` with known title pairs and expected scores
   - `normalizer` for each venue using fixture JSON
   - `router.scoreVenue` with controlled market data

3. **Add a third venue** (e.g. Manifold Markets) to demonstrate the extensibility claim in the README. Requires only client + normalizer case.

---

## Dependencies

```
github.com/google/uuid v1.6.0           — generating stable canonical IDs
github.com/sashabaranov/go-openai v1.26.0 — OpenAI embeddings API
```

No database. No web framework. No message queue. Intentionally minimal.

---

## Things that are explicitly out of scope

Do not build these even if asked:
- Real order execution or wallet integration
- Any UI (web, TUI, dashboard)
- Authentication flows
- Persistent storage / database
- Production deployment config

---

## Submission checklist

Before submitting:
- [ ] `go build ./...` passes with no errors
- [ ] `go vet ./...` passes with no warnings
- [ ] `go run ./cmd/equinox` produces routing decisions on live data
- [ ] README.md accurately describes the equivalence and routing logic
- [ ] AI usage is disclosed in README.md
- [ ] No API keys committed to git (use env vars only)
