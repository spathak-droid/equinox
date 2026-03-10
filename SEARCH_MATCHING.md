# Search-Based Cross-Venue Matching

## Problem

The original matching approach used **brute-force O(n²) pairwise comparison**: fetch ~500 markets from each venue, then compare every Polymarket market against every Kalshi market. This produced ~250,000 cross-venue pairs, almost all completely unrelated.

### Why it failed

1. **Signal drowned in noise** — comparing "Will Bitcoin hit $100k?" against "NCAA basketball spread" wastes compute and inflates the rejection rate. The few real matches are buried under 249,990+ irrelevant pairs.

2. **Title phrasing differs across venues** — the same real-world question is worded differently on each platform:
   - Polymarket: *"Will Trump win the 2024 presidential election?"*
   - Kalshi: *"Presidential Election Winner 2024"*

   Levenshtein edit distance scores these poorly because the character sequences differ, even though the meaning is identical. Jaccard keyword overlap helps but isn't enough alone.

3. **Fetched market sets barely overlap** — Polymarket's `/markets?active=true` and Kalshi's event-based fetch return whatever's popular/recent on each platform. There's no guarantee the same topics appear in both result sets. One venue might return mostly crypto markets while the other returns politics.

4. **Without embeddings, fuzzy-only matching is insufficient** — when `OPENAI_API_KEY` is not set, the composite score equals the fuzzy score. The default threshold of 0.45 is too high for differently-phrased equivalent markets, but lowering it produces too many false positives from the 250k pair space.

## Solution: Query-Based Cross-Search

Instead of "fetch all, compare all", we now do:

```
1. Fetch markets from Venue A (e.g., Polymarket)
2. Fetch markets from Venue B (e.g., Kalshi)
3. For each market from A, search Venue B using the title as a query
4. Only compare A's market against B's search results (small candidate set)
5. Repeat in reverse: for each B market, search Venue A
6. Deduplicate matched pairs
7. Fall back to brute-force if search finds nothing (e.g., mock mode)
```

### Why this works

- **Search APIs do the heavy lifting** — both Polymarket and Kalshi already have text search endpoints (`FetchMarketsByQuery`). These return only topically relevant markets, so candidates are already semantically close.

- **Reduces comparisons by ~99%** — from 250k to ~500 × 5-10 candidates = ~3,000-5,000 pairs. Each pair is far more likely to be a real match.

- **Fuzzy scoring works on pre-filtered candidates** — since search already selected topically related markets, even simple Levenshtein + Jaccard scoring can distinguish true matches from near-misses.

- **No new dependencies** — uses the existing `FetchMarketsByQuery` methods that were already implemented but never wired into the main pipeline.

## Architecture

### New files

| File | Purpose |
|------|---------|
| `internal/matcher/search.go` | Search-based matching logic |

### New types

| Type | Package | Purpose |
|------|---------|---------|
| `SearchableVenue` | `venues` | Interface extending `Venue` with `FetchMarketsByQuery` |
| `SearchResult` | `matcher` | Pairs a source market with candidates from search |
| `CrossSearchWorkerPool` | `matcher` | Bounded-concurrency worker pool for parallel search queries |
| `SearchFunc` | `matcher` | Function signature for search + normalize |

### Key functions

| Function | Purpose |
|----------|---------|
| `SearchQueryExtractor()` | Strips prediction-market boilerplate from titles to produce better search queries |
| `cleanTitleForSearch()` | Removes "Will ", "by end of year", question marks, etc. |
| `DeduplicatePairs()` | Removes duplicate pairs found from both search directions |
| `FindEquivalentPairsFromSearch()` | Runs the 4-stage scoring pipeline on search candidates |
| `RunCrossSearch()` | Parallel execution of search queries with bounded concurrency |

### Flow

```
main.go
  │
  ├── Fetch markets from each venue (FetchMarkets)
  │     └── Normalize → CanonicalMarket
  │
  ├── Cross-search phase (NEW)
  │     ├── For each Polymarket market:
  │     │     └── Search Kalshi by cleaned title → normalize results
  │     ├── For each Kalshi market:
  │     │     └── Search Polymarket by cleaned title → normalize results
  │     └── Bounded concurrency (8 workers)
  │
  ├── Score candidate pairs (FindEquivalentPairsFromSearch)
  │     ├── Stage 1: Hard filters (active status, date proximity)
  │     ├── Stage 2: Fuzzy title score
  │     ├── Stage 3: Embedding cosine similarity (if available)
  │     ├── Stage 4: LLM disambiguation (if available)
  │     └── Deduplicate pairs
  │
  ├── Fallback to brute-force if search found nothing
  │
  └── Route matched pairs → decisions
```

### Title cleaning examples

| Raw title | Cleaned query |
|-----------|--------------|
| "Will Trump win the 2024 presidential election?" | "Trump win the 2024 presidential election" |
| "Will Bitcoin reach $100,000 by end of year?" | "Bitcoin reach $100,000" |
| "Is inflation going to rise in 2025?" | "Inflation going to rise" |
| "What is the probability that the Fed cuts rates?" | "The fed cuts rates" |

## Deduplication & False Positive Prevention

### Source Market Deduplication (DiversifySourceMarkets)

Polymarket often has 30+ variants of the same question pattern (e.g., "Will [Team X] win the 2026 NBA Finals?" for every NBA team). Instead of sending 30 near-identical search queries, `DiversifySourceMarkets` groups markets by their structural pattern using `extractCorePattern()` and picks one representative per group (the highest-liquidity market).

Example: 30 NBA Finals markets → 1 search query.

### Rate Limiting

Kalshi's API returns 429 errors under high concurrency. The `CrossSearchWorkerPool` now uses:
- `Concurrency: 3` (down from 8)
- `DelayBetweenQueries: 300ms` ticker-based rate limiting

### Entity Mismatch Penalty

Without this, "Will Oprah Winfrey win the 2028 Democratic nomination?" false-matches against "Will Hunter Biden win the 2028 Democratic nomination?" because they share ~80% keywords.

`entityMismatchPenalty()` in `matcher.go` detects when two titles share a structural template but differ in the key entity (the subject). It computes a `templateRatio = shared_words / (shared + unique_words)`:
- templateRatio > 0.6 → penalty of 0.25 (template swap, different subjects)
- templateRatio > 0.4 → penalty of 0.15

This prevents high Jaccard scores from creating false matches on "same question template, different person/team" pairs.

### Candidate-Level Deduplication

`deduplicateSearchResults()` runs before scoring. If 10 source markets all found the same Kalshi candidate via search, it keeps only the source with the highest fuzzy title similarity. This reduced raw pairs from 954 → 416 unique pairs in testing.

## Changes to existing files

### `internal/venues/venue.go`
- Added `SearchableVenue` interface that extends `Venue` with `FetchMarketsByQuery(ctx, query) ([]*RawMarket, error)`
- Both `polymarket.Client` and `kalshi.Client` already implement this method

### `cmd/equinox/main.go`
- Venue initialization now tracks `SearchableVenue` implementations
- Markets stored by venue for directional cross-search
- New cross-search phase before brute-force fallback
- Brute-force kept as fallback for mock mode or when search finds nothing

## Configuration

No new config variables. The search approach uses existing:
- `MATCH_THRESHOLD` — composite score for confirmed matches
- `PROBABLE_MATCH_THRESHOLD` — composite score for ambiguous pairs sent to LLM
- `OPENAI_API_KEY` — enables embedding + LLM stages (search works without it too)

## Constraints preserved

- **Venue clients** still only fetch raw JSON → `RawMarket`
- **Normalizer** still the only package with venue-specific parsing
- **Matcher** still venue-agnostic, operates on `CanonicalMarket` only
- **Router** unchanged
- **Adding a new venue** still requires only: client + normalizer case + main.go registration (plus optionally implementing `SearchableVenue` for cross-search)
