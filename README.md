# Project Equinox

**Cross-Venue Prediction Market Aggregation & Routing Simulation**

A working prototype that detects equivalent markets across Polymarket and Kalshi,
normalizes them into a shared internal representation, and simulates routing decisions
for hypothetical trades. This is an infrastructure prototype, not a trading product.

---

## Quick Start

```bash
# Clone and install deps
git clone <repo>
cd equinox
go mod tidy

# Run with AI-assisted matching (recommended)
OPENAI_API_KEY=sk-... go run ./cmd/equinox

# Run with rule-based matching only (no API key required)
go run ./cmd/equinox

# Run UI
go run ./cmd/equinox-ui

# Optional: Kalshi auth (enables more pricing data)
KALSHI_API_KEY=... OPENAI_API_KEY=sk-... go run ./cmd/equinox

# Route the first 10 matched pairs on YES
go run ./cmd/equinox -mode=route -side=YES -max-pairs=10

# Output machine-readable results
go run ./cmd/equinox -mode=match -output=json

# Offline deterministic run from testdata fixture
go run ./cmd/equinox -mock -mock-path=testdata/markets.mock.json -mode=match -output=json

# Write JSON output to file
go run ./cmd/equinox -mode=route -output=json -output-file=/tmp/equinox-route.json
```

## Fixture format (mock mode)

`testdata/markets.mock.json` is expected to be either:

- Venue map format:

```json
{
  "polymarket": [/* raw Polymarket JSON items */],
  "kalshi": [/* raw Kalshi JSON items */]
}
```

- Flat list format:

```json
[
  { "venue_id": "polymarket", "payload": { ... } },
  { "venue_id": "kalshi", "payload": { ... } }
]
```

**Requirements:** Go 1.21+. No database, no external services beyond the two venue APIs and optionally OpenAI.

---

## Configuration

All config is via environment variables. Defaults are production-ready for a prototype.

| Variable | Default | Description |
|---|---|---|
| `OPENAI_API_KEY` | _(empty)_ | Enables embedding-based matching. Falls back to rules-only if unset. |
| `KALSHI_API_KEY` | _(empty)_ | Required for Kalshi auth endpoints. Public markets work without it. |
| `MATCH_THRESHOLD` | `0.45` | Composite score above which markets are declared equivalent. |
| `PROBABLE_MATCH_THRESHOLD` | `0.35` | Score range flagged for disambiguation review. |
| `MAX_DATE_DELTA_DAYS` | `365` | Markets resolving more than this many days apart are never matched. |
| `PRICE_WEIGHT` | `0.60` | Routing weight assigned to price quality. |
| `LIQUIDITY_WEIGHT` | `0.30` | Routing weight assigned to available liquidity. |
| `SPREAD_WEIGHT` | `0.10` | Routing weight assigned to bid-ask spread. |
| `DEFAULT_ORDER_SIZE` | `1000.0` | Hypothetical order size in USD for simulation. |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | OpenAI embedding model name. |
| `EMBEDDING_CACHE_ENABLED` | `false` | Enable local disk cache for title embeddings to reduce repeat OpenAI calls. |
| `EMBEDDING_CACHE_PATH` | `.equinox_embedding_cache.json` | Location of optional embedding cache file. |
| `HTTP_TIMEOUT_SECONDS` | `30` | Venue API HTTP timeout. |

---

## UI Modes

The UI supports two fetch modes via the top selector:

- `search` (default): uses venue search APIs and requires a query (`q`).
  - Polymarket: `GET /public-search?cache=true&q=...`
  - Kalshi: `GET /markets?status=open&search=...`
- `broad`: ingests open markets from both venues (non-search pagination).

Use URL parameters directly if needed:

- `http://localhost:8080/?mode=search&q=nepal+election`
- `http://localhost:8080/?mode=broad`

---

## Kalshi Search Wrapper API

The UI server also exposes a wrapper endpoint that simulates a true keyword search for Kalshi:

`GET /api/kalshi-search?q=bitcoin&status=open&limit=20`

Optional filters:

- `type=market|event|series`
- `series=<series_ticker>`
- `event=<event_ticker>`

Response shape:

```json
{
  "query": "bitcoin",
  "count": 2,
  "results": [
    {
      "id": "market:KXBTC-26MAR10-T100K",
      "type": "market",
      "ticker": "KXBTC-26MAR10-T100K",
      "title": "Will Bitcoin touch 100,000 by March 10?",
      "subtitle": "Crypto / Bitcoin",
      "status": "open",
      "event_ticker": "KXBTC-26MAR10",
      "series_ticker": "KXBTC",
      "volume": 12345,
      "liquidity": 67890,
      "score": 167.4
    }
  ]
}
```

Notes:

- The wrapper pulls discovery data from `/markets`, `/events`, and `/series`.
- Results are normalized into one shape and ranked locally.
- In-memory cache avoids re-scanning Kalshi on every request.

---

## Architecture

```
cmd/equinox/main.go          Entry point — wires all layers together

config/                      Configuration loading from env vars

internal/
  models/canonical.go        Canonical market schema (venue-agnostic)
  venues/
    venue.go                 Venue interface
    polymarket/client.go     Polymarket Gamma API client
    kalshi/client.go         Kalshi REST API client
  normalizer/normalizer.go   Transforms raw → canonical; calls OpenAI embeddings
  matcher/matcher.go         Equivalence detection pipeline
  router/router.go           Venue-agnostic routing engine
```

### Layer responsibilities

| Layer | Knows about venues? | Knows about routing? |
|---|---|---|
| Venue clients | Yes (reads venue API) | No |
| Normalizer | Yes (parses venue schemas) | No |
| Canonical model | No | No |
| Matcher | No | No |
| Router | No | Yes |

This separation ensures that adding a third venue (e.g. Manifold) requires only:
1. A new client implementing `venues.Venue`
2. A new normalizer case in `normalizer.go`
3. Registration in `main.go`

The matcher and router require **zero changes**.

---

## Equivalence Detection

### Definition

Two markets are considered equivalent when:
1. They represent the **same binary question** (e.g. "Will X win election Y?")
2. Their YES outcomes refer to the **same real-world result**
3. They are expected to resolve within **`MAX_DATE_DELTA_DAYS`** of each other

### Methodology

We use a four-stage pipeline:

**Stage 1 — Hard filters** _(applied first; fast, zero cost)_
- Both markets must be active
- If both have resolution dates: `|dateA - dateB| <= MAX_DATE_DELTA_DAYS`
- Markets missing resolution dates are not date-filtered but may still match on content

**Stage 2 — Fuzzy title similarity** _(always applied)_
- Normalized edit distance (Levenshtein) on cleaned titles
- Keyword Jaccard overlap after stopword removal
- `fuzzyScore = 0.5 × editSim + 0.5 × jaccardSim`

**Stage 3 — Embedding cosine similarity** _(applied when OPENAI_API_KEY is set)_
- OpenAI `text-embedding-3-small` embeddings of market `title` text
- Batched in a single API call to minimize cost
- `cosineSimilarity(embeddingA, embeddingB)`
- Skipped when no API key; matcher falls back to fuzzy-only

**Stage 4 — LLM pairwise disambiguation** _(applied when OPENAI_API_KEY is set)_
- Runs only for pairs where `composite` is in `[PROBABLE_MATCH_THRESHOLD, MATCH_THRESHOLD)`
- If title embeddings were unavailable, we still attempt disambiguation for ambiguous pairs when OpenAI is configured.
- OpenAI returns `match`, `no_match`, or `unsure` per pair
- `match` upgrades confidence to MATCH
- `no_match` removes the pair
- `unsure` keeps it as PROBABLE_MATCH

**Composite score:**
```
if embeddings available:  composite = 0.40 × fuzzy + 0.60 × embedding
if embeddings absent:     composite = fuzzy
```

**Classification:**
```
composite >= MATCH_THRESHOLD          → MATCH
composite >= PROBABLE_MATCH_THRESHOLD → PROBABLE_MATCH (or MATCH after LLM disambiguation when OPENAI_API_KEY is set)
else                                  → NO_MATCH
```

### Why this approach?

- **Rules alone** are brittle to paraphrasing: "Will the Fed cut rates?" vs "Federal Reserve rate cut in June?" scores low on edit distance but high on embedding similarity.
- **Embeddings alone** can match semantically related but non-equivalent markets (e.g. two different Fed meetings). The date filter is the critical guard.
- **Hybrid** gives us the best of both: cheap rules eliminate obvious non-matches early, embeddings handle paraphrasing and abbreviations.

### Known failure modes

| Scenario | Behavior | Mitigation |
|---|---|---|
| Monthly rolling contracts (same question, different dates) | May match if dates are close | Reduce MAX_DATE_DELTA_DAYS |
| Short generic titles ("Will inflation rise?") | High fuzzy score regardless of date | Date filter is primary guard |
| Markets with no resolution date | Excluded from date filtering | Still matched via content score |
| Venue returns malformed JSON | Market skipped, warning logged | Partial failure is acceptable |

---

## Routing Logic

For each equivalent market pair, the router selects the better venue for a hypothetical order.

### Scoring model

For each candidate venue, we compute:

```
priceScore:
  BUY YES → 1 - yes_price    (lower price = higher score)
  BUY NO  → yes_price        (higher yes_price → lower no_price)

liquidityScore:
  tanh(liquidity / order_size)   ∈ [0, 1)
  — saturates smoothly; penalizes insufficient liquidity without hard cutoff

spreadScore:
  if spread reported:  1 - min(spread / 0.20, 1.0)
  if spread = 0:       0.5 (neutral — data unavailable)

routingScore = PriceWeight × price + LiquidityWeight × liquidity + SpreadWeight × spread
```

The venue with the highest `routingScore` is selected. Ties broken by index (deterministic).

### Sample output

```
═══════════════════════════════════════════════════════════
ROUTING DECISION
═══════════════════════════════════════════════════════════
Order:   YES Will the Fed cut rates in June? @ $1000.00
Markets: polymarket / kalshi
Match confidence: MATCH (score=0.891)

Venue scores:
  [polymarket] total=0.6142 | yes_price=0.6200 (score=0.3800) | liquidity=$42000 (score=0.9999) | spread=N/A (score=0.5 neutral)
▶ [kalshi]     total=0.6731 | yes_price=0.5800 (score=0.4200) | liquidity=$18000 (score=0.9999) | spread=0.0400 (score=0.800)

Weights: price=60% liquidity=30% spread=10%

✅ SELECTED: kalshi (score=0.6731)
   Title: Fed Rate Cut June 2025
   Yes price: 0.5800 | Liquidity: $18000 | Spread: 0.0400
═══════════════════════════════════════════════════════════
```

### Tradeoffs

- Price is weighted highest (60%) because in a frictionless simulation, price is the primary determinant of execution quality.
- Liquidity (30%) ensures we don't route to a venue that can't absorb the order — but we don't hard-exclude venues with insufficient liquidity, since partial fills may be acceptable.
- Spread (10%) is a secondary quality signal; many venues don't report it, so it's weighted low to avoid penalizing venues unfairly.

---

## Assumptions & Limitations

- **No real execution.** This is a simulation. No orders are placed.
- **Polymarket spread unavailable** from the summary API endpoint. Spread is set to 0 (neutral) and noted.
- **Categories are lossy.** Venue-specific categories are mapped to a small controlled vocabulary. Mismatches are possible.
- **Embedding batching.** All markets are embedded in a single API call. Very large datasets (>2000 markets) would require chunking.
- **Mostly no persistence.** Pipeline outputs are produced in-memory for the run; optional embedding cache can persist vectors for unchanged titles between runs to cut repeat OpenAI calls.
- **Routing weights are configurable** but not auto-tuned. A production system would A/B test weight combinations against historical execution quality.

---

## AI Usage Disclosure

OpenAI's `text-embedding-3-small` model is used to compute dense vector representations of market titles. These embeddings enable semantic matching that is robust to paraphrasing and abbreviation differences across venues.

The matcher uses embeddings in stage 3 and optionally performs a lightweight LLM disambiguation pass in stage 4 for probable matches. The AI component is optional — the system falls back to rule-based matching when no API key is configured, which is documented behavior, not a failure state.

AI was also used during development as a coding assistant (GitHub Copilot / Claude) for boilerplate and documentation.

---

## Development & Testing

```bash
# Run tests
go test ./...

# Lint
go vet ./...

# Build binary
go build -o bin/equinox ./cmd/equinox
```

---

## What's out of scope

Per the project specification:
- Real-money trading or order execution
- Wallet or authentication integration
- Regulatory compliance
- Production UI
- Performance optimization

---

*Project Equinox — Infrastructure Prototype — Not for production use.*
