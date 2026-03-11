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

# Run the built-in LLM matching eval suite
OPENAI_API_KEY=sk-... go run ./cmd/equinox -mode=llm-eval

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
| `OPEN_AI_MODEL` | `gpt-4o-mini` | OpenAI chat model used for LLM-assisted pair disambiguation. |
| `LLM_MIN_CONFIDENCE` | `0.80` | Minimum LLM confidence required before a `match` decision is accepted. |
| `ROUTER_USE_LLM` | `false` | If `true`, asks the LLM to judge final venue routing using price/liquidity/spread/volume + weights. |
| `ROUTER_LLM_MIN_CONFIDENCE` | `0.70` | Minimum confidence required to accept LLM routing choice; otherwise deterministic router is used. |
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
1. They represent the **same binary question** (e.g. "Will Chelsea win the Premier League?")
2. Their YES outcomes refer to the **same real-world result**
3. They are expected to resolve within **`MAX_DATE_DELTA_DAYS`** of each other

This is harder than it sounds. Polymarket asks "Will Chelsea win the 2025-26 English Premier League?" while Kalshi phrases the same question as "English Premier League Winner? — Chelsea". Edit distance says these are different strings. A human instantly sees they're the same bet.

### Discovery: cross-search matching

The naive approach to cross-venue matching is O(n^2): compare every Polymarket market against every Kalshi market. With 100 markets per venue, that's 10,000 comparisons — most of which are obviously unrelated.

Instead, we use **cross-search**: each venue's markets become search queries against the other venue's API. A Polymarket market about Chelsea triggers a Kalshi search for "Chelsea Premier League", which returns only the handful of relevant Kalshi markets. This reduces candidate pairs from ~10,000 to ~50-100, with much higher signal density.

The cross-search pipeline (`matcher/search.go`):
1. Clean each market title into a search query (strip "Will", question marks, date suffixes)
2. Search the opposing venue's API with that query
3. Rank results by token Jaccard similarity to the source title
4. Send top candidates through the scoring pipeline

Fallback: when search APIs are unavailable (mock mode, API errors), we fall back to brute-force Jaccard cross-pollination over the full market pools.

### Scoring pipeline

Once candidate pairs are identified, each pair runs through a multi-signal scoring pipeline:

**Signal 1 — Hard filters** _(applied first; fast, zero cost)_
- Both markets must be active
- If both have resolution dates: `|dateA - dateB| <= MAX_DATE_DELTA_DAYS`

**Signal 2 — Fuzzy title similarity** _(always applied)_
- Normalized Levenshtein distance + keyword Jaccard overlap after stopword removal
- `fuzzyScore = 0.5 * editSim + 0.5 * jaccardSim`

**Signal 3 — Entity overlap** _(always applied)_
- Extract named entities from both titles (proper nouns, numbers, known synonyms)
- Jaccard overlap of entity sets
- This catches pairs like "Chelsea win Premier League" vs "Premier League Winner — Chelsea" that have low fuzzy scores but share all key entities

**Signal 4 — Embedding cosine similarity** _(when OPENAI_API_KEY is set)_
- OpenAI `text-embedding-3-small` vectors, batched in a single API call
- Handles paraphrasing and abbreviation differences that string matching misses

**Signal 5 — LLM pairwise disambiguation** _(when OPENAI_API_KEY is set)_
- Ambiguous pairs are sent to a chat model for pairwise `match` / `no_match` / `unsure`
- LLM confidence is downgraded proportionally when fuzzy/entity signals are weak — this prevents the LLM from hallucinating matches between topically-related but non-equivalent markets
- Requires at least one corroborating signal (entity overlap >= 0.40, event match >= 0.60, or rule composite >= 0.35) before accepting an LLM match

**Composite score:**
```
with embeddings:    composite = 0.40 * fuzzy + 0.60 * embedding
without embeddings: composite = fuzzy
```

**Classification:**
```
composite >= MATCH_THRESHOLD (0.45)          → MATCH
composite >= PROBABLE_MATCH_THRESHOLD (0.35) → PROBABLE_MATCH
else                                         → NO_MATCH
```

LLM disambiguation can upgrade PROBABLE_MATCH to MATCH or downgrade to NO_MATCH.

### Why not just use embeddings? Why not just use an LLM?

Each signal alone has failure modes. The hybrid approach uses cheap signals to filter and expensive signals to disambiguate:

- **Fuzzy matching alone** fails on paraphrasing: "Will the Fed cut rates?" vs "Federal Reserve rate reduction in June?" scores low on edit distance despite being the same question.
- **Embeddings alone** match semantically related but non-equivalent markets. "Fed rate cut March 2026" and "Fed rate cut June 2026" embed almost identically — the date filter is the critical guard.
- **LLM alone** is expensive and can hallucinate. Sending every possible pair through GPT-4 is neither practical nor reliable. We observed the LLM confidently matching "Will Chelsea win the Premier League?" with "Will Arsenal win the Premier League?" (0.90 confidence) because both are about the same league. The entity overlap gate catches this — Chelsea != Arsenal.
- **The hybrid** uses rules to eliminate 95% of candidates cheaply, embeddings to handle paraphrasing, entity overlap to verify subject identity, and the LLM as a final tiebreaker only where signals disagree.

### Known failure modes

| Scenario | Behavior | Mitigation |
|---|---|---|
| Same question, different time periods (monthly rolling) | May match if dates are close | Reduce `MAX_DATE_DELTA_DAYS` |
| Generic titles ("Will inflation rise?") | False positive risk from high fuzzy scores | Date filter + entity gate |
| Categorical vs binary framing | "EPL Winner — Chelsea" vs "Will Chelsea win EPL?" | Entity overlap + LLM disambiguation |
| Markets with no resolution date | Cannot date-filter | Must rely on content signals alone |
| Venue API returns errors | Markets from that venue skipped | Partial results, never a crash |

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

From a live run matching "Will Chelsea win the 2025-26 English Premier League?" across Polymarket and Kalshi:

```
═══════════════════════════════════════════════════════════
ROUTING DECISION
═══════════════════════════════════════════════════════════
Order:   BUY YES on "Will Chelsea win the 2025–26 English Premier League?" for $1000
Markets: polymarket vs kalshi
Match:   PROBABLE_MATCH (confidence=0.519)

── Venue Comparison ────────────────────────────────────
▶ [polymarket] score=0.9972
    Price:     YES share costs $0.0030 → $1000 buys ~333333 shares
    Liquidity: $403508 available ✓ covers $1000 order fully
    Spread:    0.0020 (20 bps) — very tight
  [kalshi] score=0.9920
    Price:     YES share costs $0.0050 → $1000 buys ~200000 shares
    Liquidity: $2628943 available ✓ covers $1000 order fully
    Spread:    0.0100 (100 bps) — reasonable

── Weights ─────────────────────────────────────────────
   Price=60%  Liquidity=30%  Spread=10%

── Why polymarket? ─────────────────────────────────────
   1. Better price: YES shares cost $0.0030 vs $0.0050 on kalshi — 40.0% cheaper.
   2. Liquidity is lower ($403508 vs $2628943), but price advantage outweighs this.
   3. Tighter spread: 20 bps vs 100 bps — lower hidden execution cost.

── Estimated Execution ─────────────────────────────────
   Venue:           polymarket
   Side:            BUY YES
   Cost per share:  $0.0030
   Order size:      $1000
   Shares:          ~333333
   If correct:      $333333 payout ($332333 profit, 33233% return)
═══════════════════════════════════════════════════════════
```

The router doesn't just pick a venue — it explains *why* in plain English, breaking down each factor's contribution and estimating concrete execution outcomes.

---

## Design Decisions & Tradeoffs

### Why cross-search instead of brute-force matching?

Brute-force O(n^2) comparison works in a prototype with 20 markets per venue. It doesn't work with 500+. Cross-search reduces the problem from "compare everything" to "for each market, ask the other venue what's similar." This is the same approach a human would take — you wouldn't read every Kalshi market to find the one that matches a Polymarket question. You'd search.

The tradeoff: cross-search depends on venue search API quality. If Kalshi's search doesn't return a relevant result, we miss the pair. We mitigate this with a brute-force Jaccard fallback after search completes.

### Why weight price at 60%, liquidity at 30%, spread at 10%?

Price is the dominant factor because in a frictionless simulation, getting a better price per share directly determines returns. A 2-cent price difference on a $1000 order matters more than a liquidity difference when both venues can fill the order.

Liquidity at 30% ensures we penalize venues that can't absorb the order size — but we don't hard-exclude them. A venue with $500 liquidity for a $1000 order gets a low score but isn't rejected, because partial fills may be acceptable and liquidity fluctuates.

Spread at 10% because many venues don't report it. Polymarket's summary API doesn't include spread; Kalshi does. Weighting spread heavily would systematically penalize Polymarket for missing data, not for bad execution quality. We score missing spread as 0.5 (neutral) rather than 0.0 (worst) for the same reason.

These weights are configurable via environment variables. A production system would A/B test weight combinations against historical execution data.

### Why tanh for liquidity scoring?

`tanh(liquidity / order_size)` produces a smooth [0, 1) curve that:
- Returns ~0 when liquidity is near zero
- Returns ~0.5 when liquidity equals order size
- Saturates near 1.0 when liquidity far exceeds order size

This avoids the cliff-edge problem of a hard threshold ("reject if liquidity < order size") while still meaningfully penalizing thin venues. The alternative — linear scaling — would give a venue with $10M liquidity a 10x better score than one with $1M, which overstates the difference for a $1000 order. tanh captures diminishing returns.

### Why require corroborating signals for LLM matches?

Early testing showed the LLM confidently matching topically-related but non-equivalent markets. "Will Chelsea win the EPL?" and "Will Arsenal win the EPL?" both discuss the same league, same season, same sport — an LLM sees high semantic similarity. But they're opposite bets.

We require at least one of: entity overlap >= 0.40, event match score >= 0.60, rule composite >= 0.35, or a signature match. This ensures the LLM's judgment is backed by at least one structural signal, not just vibes.

The tradeoff: this makes the system more conservative. Some genuine matches with unusual title structures may be classified as PROBABLE_MATCH instead of MATCH. We accept this — a false negative (missed match) is far less costly than a false positive (routing a trade to the wrong market).

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
