package matcher

import (
	"sort"
	"strings"

	"github.com/equinox/internal/models"
)

// tokenJaccard computes word-level Jaccard similarity between two strings
// WITHOUT stopword removal. This is used for quick pre-filtering where we want
// raw token overlap rather than the semantic-aware keywordJaccard from matcher.go.
func tokenJaccard(a, b string) float64 {
	wa := strings.Fields(strings.ToLower(a))
	wb := strings.Fields(strings.ToLower(b))
	setA := make(map[string]bool, len(wa))
	for _, w := range wa {
		setA[w] = true
	}
	inter, union := 0, len(setA)
	for _, w := range wb {
		if setA[w] {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// SearchQueryExtractor returns the best search query for a market.
// For Kalshi markets with composite titles ("Event — Subtitle"), we use
// the event title since that's the actual topic. For everything else,
// we use the market title as-is and let the search APIs handle relevance.
func SearchQueryExtractor(m *models.CanonicalMarket) string {
	// If we have a venue event title (e.g. Kalshi's event_title), prefer it
	// as the search query — it's the clean topic without market-specific suffixes.
	if m.VenueEventTitle != "" {
		return strings.TrimSpace(m.VenueEventTitle)
	}
	return strings.TrimSpace(m.Title)
}

// DeduplicatePairs removes duplicate match results (same market pair regardless of order).
func DeduplicatePairs(pairs []*MatchResult) []*MatchResult {
	type pairKey struct {
		a, b string // VenueMarketIDs, sorted
	}
	seen := map[pairKey]bool{}
	var out []*MatchResult

	for _, p := range pairs {
		idA := p.MarketA.VenueMarketID
		idB := p.MarketB.VenueMarketID
		// Normalize key order
		k := pairKey{a: idA, b: idB}
		if idA > idB {
			k = pairKey{a: idB, b: idA}
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	return out
}

// deduplicateSearchResults collapses duplicate source→candidate pairs before scoring.
// When multiple source markets (e.g. 10 Polymarket Bitcoin markets) each find the same
// Kalshi candidate via search, we only need to score each unique (source, candidate) pair
// once. We keep the source with the highest fuzzy title similarity to the candidate,
// since that's the most meaningful comparison.
func deduplicateSearchResults(searchResults []SearchResult) []SearchResult {
	// First, collect all unique candidates across all search results
	type candidateKey struct {
		sourceVenue models.VenueID
		candidateID string
	}

	// Group: for each unique candidate, find the best source market to pair it with
	type bestPair struct {
		source    *models.CanonicalMarket
		candidate *models.CanonicalMarket
		fuzzy     float64
	}
	best := map[candidateKey]*bestPair{}

	for _, sr := range searchResults {
		for _, candidate := range sr.Candidates {
			if sr.Source.VenueID == candidate.VenueID {
				continue
			}
			key := candidateKey{
				sourceVenue: sr.Source.VenueID,
				candidateID: candidate.VenueMarketID,
			}
			score := fuzzyTitleScore(sr.Source.Title, candidate.Title)
			if existing, ok := best[key]; !ok || score > existing.fuzzy {
				best[key] = &bestPair{
					source:    sr.Source,
					candidate: candidate,
					fuzzy:     score,
				}
			}
		}
	}

	// Also deduplicate candidates themselves (same market returned by different queries)
	seenCandidates := map[string]*models.CanonicalMarket{}
	for _, bp := range best {
		if existing, ok := seenCandidates[bp.candidate.VenueMarketID]; ok {
			// Keep the one with more data (higher liquidity as proxy)
			if bp.candidate.Liquidity > existing.Liquidity {
				seenCandidates[bp.candidate.VenueMarketID] = bp.candidate
			}
		} else {
			seenCandidates[bp.candidate.VenueMarketID] = bp.candidate
		}
	}

	// Rebuild deduplicated search results: one SearchResult per unique (source, candidate)
	var out []SearchResult
	seen := map[string]bool{}
	for _, bp := range best {
		// Use the canonical version of the candidate
		bp.candidate = seenCandidates[bp.candidate.VenueMarketID]

		pairKey := bp.source.VenueMarketID + "|" + bp.candidate.VenueMarketID
		if seen[pairKey] {
			continue
		}
		seen[pairKey] = true

		// Check if this source already has a SearchResult
		found := false
		for i := range out {
			if out[i].Source.VenueMarketID == bp.source.VenueMarketID {
				out[i].Candidates = append(out[i].Candidates, bp.candidate)
				found = true
				break
			}
		}
		if !found {
			out = append(out, SearchResult{
				Source:     bp.source,
				Candidates: []*models.CanonicalMarket{bp.candidate},
			})
		}
	}

	return out
}

// deduplicateByMarket ensures each individual market ID appears in at most one
// MatchResult. When a market appears in multiple pairs, only the highest-scoring
// pair (first after sorting) is kept.
func deduplicateByMarket(pairs []*MatchResult) []*MatchResult {
	usedA := map[string]bool{}
	usedB := map[string]bool{}
	var out []*MatchResult
	for _, p := range pairs {
		idA := p.MarketA.VenueMarketID
		idB := p.MarketB.VenueMarketID
		if usedA[idA] || usedB[idB] || usedA[idB] || usedB[idA] {
			continue
		}
		usedA[idA] = true
		usedB[idB] = true
		out = append(out, p)
	}
	return out
}

// topPairsByJaccard scores every marketA×marketB combination by Jaccard title
// similarity and returns the top-k unique pairs (each market used at most once).
func topPairsByJaccard(marketsA, marketsB []*models.CanonicalMarket, k int) [][2]*models.CanonicalMarket {
	type pair struct {
		marketA *models.CanonicalMarket
		marketB *models.CanonicalMarket
		sim     float64
	}

	var all []pair
	for _, a := range marketsA {
		for _, b := range marketsB {
			all = append(all, pair{a, b, tokenJaccard(a.Title, b.Title)})
		}
	}

	// Sort descending by similarity
	sort.Slice(all, func(i, j int) bool { return all[i].sim > all[j].sim })

	// Take top-k, each market used once
	usedA := map[string]bool{}
	usedB := map[string]bool{}
	var result [][2]*models.CanonicalMarket
	for _, pr := range all {
		if len(result) >= k {
			break
		}
		if usedA[pr.marketA.VenueMarketID] || usedB[pr.marketB.VenueMarketID] {
			continue
		}
		usedA[pr.marketA.VenueMarketID] = true
		usedB[pr.marketB.VenueMarketID] = true
		result = append(result, [2]*models.CanonicalMarket{pr.marketA, pr.marketB})
	}
	return result
}

// topByQueryMatch scores markets by how many query words appear in their title
// and returns the top-k. Markets with zero query-word overlap are excluded.
func topByQueryMatch(query string, markets []*models.CanonicalMarket, k int) []*models.CanonicalMarket {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 || len(markets) == 0 {
		if k > len(markets) {
			return markets
		}
		return markets[:k]
	}

	type scored struct {
		m     *models.CanonicalMarket
		score float64
	}
	items := make([]scored, 0, len(markets))
	for _, mm := range markets {
		title := strings.ToLower(mm.Title)
		matched := 0
		for _, w := range words {
			if strings.Contains(title, w) {
				matched++
			}
		}
		if matched > 0 {
			items = append(items, scored{mm, float64(matched) / float64(len(words))})
		}
	}

	// Sort descending by score
	sort.Slice(items, func(i, j int) bool { return items[i].score > items[j].score })

	if k > len(items) {
		k = len(items)
	}
	out := make([]*models.CanonicalMarket, k)
	for i := range out {
		out[i] = items[i].m
	}
	return out
}

func countRawPairs(searchResults []SearchResult) int {
	n := 0
	for _, sr := range searchResults {
		n += len(sr.Candidates)
	}
	return n
}

// groupByVenue collects all unique markets from search results, grouped by VenueID.
func groupByVenue(searchResults []SearchResult) map[models.VenueID][]*models.CanonicalMarket {
	seen := map[models.VenueID]map[string]*models.CanonicalMarket{}
	addMarket := func(m *models.CanonicalMarket) {
		if _, ok := seen[m.VenueID]; !ok {
			seen[m.VenueID] = map[string]*models.CanonicalMarket{}
		}
		seen[m.VenueID][m.VenueMarketID] = m
	}
	for _, sr := range searchResults {
		addMarket(sr.Source)
		for _, c := range sr.Candidates {
			addMarket(c)
		}
	}
	result := make(map[models.VenueID][]*models.CanonicalMarket, len(seen))
	for vid, mm := range seen {
		markets := make([]*models.CanonicalMarket, 0, len(mm))
		for _, m := range mm {
			markets = append(markets, m)
		}
		result[vid] = markets
	}
	return result
}
