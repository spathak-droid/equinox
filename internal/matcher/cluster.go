// Package matcher — cluster.go implements topic-based clustering for markets.
//
// The pipeline is: fetch broadly → normalize → cluster by topic → match within clusters.
//
// Clustering groups markets that share entities, keywords, and categories into
// topic buckets. Matching then only happens within clusters, which:
//   - Dramatically reduces the number of pairwise comparisons
//   - Improves precision by only comparing topically related markets
//   - Works entirely without AI (no embeddings, no LLM)
//
// A market may belong to multiple clusters (e.g., "Will Bitcoin hit $100k before
// the Fed cuts rates?" belongs to both a "Bitcoin" cluster and a "Fed rate" cluster).
// The matching pipeline deduplicates pairs found across clusters.
package matcher

import (
	"sort"
	"strings"

	"github.com/equinox/internal/models"
)

// TopicCluster groups markets that share a common topic signature.
type TopicCluster struct {
	ID        string                    // deterministic key from signature
	Label     string                    // human-readable label (e.g., "Bitcoin (crypto)")
	Signature TopicSignature            // what defines this cluster
	Markets   []*models.CanonicalMarket // all markets in this cluster
}

// TopicSignature defines what makes markets "about the same thing."
type TopicSignature struct {
	Category string   // normalized category (e.g., "crypto", "politics")
	Entities []string // sorted lowercased proper nouns (e.g., ["bitcoin", "trump"])
}

// HasCrossVenue returns true if the cluster contains markets from at least 2 venues.
func (c *TopicCluster) HasCrossVenue() bool {
	if len(c.Markets) < 2 {
		return false
	}
	first := c.Markets[0].VenueID
	for _, m := range c.Markets[1:] {
		if m.VenueID != first {
			return true
		}
	}
	return false
}

// VenueBreakdown returns market counts per venue.
func (c *TopicCluster) VenueBreakdown() map[models.VenueID]int {
	counts := map[models.VenueID]int{}
	for _, m := range c.Markets {
		counts[m.VenueID]++
	}
	return counts
}

// signatureKey produces a deterministic string key for grouping.
func signatureKey(sig TopicSignature) string {
	return sig.Category + "|" + strings.Join(sig.Entities, ",")
}

// signatureLabel generates a human-readable label from a signature.
func signatureLabel(sig TopicSignature) string {
	var parts []string
	for _, e := range sig.Entities {
		// Title-case each entity
		if len(e) > 0 {
			parts = append(parts, strings.ToUpper(e[:1])+e[1:])
		}
	}
	label := strings.Join(parts, " / ")
	if sig.Category != "" && sig.Category != "other" {
		if label != "" {
			label += " (" + sig.Category + ")"
		} else {
			label = sig.Category
		}
	}
	if label == "" {
		label = "general"
	}
	return label
}

// ClusterByTopic groups markets into topic clusters based on their entities
// and categories. Markets with shared entities are placed into the same cluster.
// A market may appear in multiple clusters if it has multiple entities.
//
// The algorithm:
//  1. Extract entities from each market title
//  2. For each entity, create/find a cluster keyed by (category, entity)
//  3. Assign the market to every matching entity cluster
//  4. Markets with no entities go into a category-only cluster
//  5. Merge small single-venue clusters into broader category clusters
func ClusterByTopic(markets []*models.CanonicalMarket) []*TopicCluster {
	type clusterEntry struct {
		sig     TopicSignature
		markets map[string]*models.CanonicalMarket // venueMarketID → market (dedup)
	}

	clusters := map[string]*clusterEntry{}
	var order []string // preserve insertion order

	for _, m := range markets {
		entities := extractEntities(m.Title)

		// Also extract entities from normalized title (catches synonyms like "btc" → "bitcoin")
		normEntities := extractNormKeyEntities(m.Title)
		for _, e := range normEntities {
			entities = append(entities, e)
		}

		// Deduplicate entities
		entSet := map[string]bool{}
		var uniqueEntities []string
		for _, e := range entities {
			e = strings.ToLower(e)
			if !entSet[e] {
				entSet[e] = true
				uniqueEntities = append(uniqueEntities, e)
			}
		}
		sort.Strings(uniqueEntities)

		cat := m.Category
		if cat == "" {
			cat = "other"
		}

		if len(uniqueEntities) == 0 {
			// No entities — assign to category-only cluster
			sig := TopicSignature{Category: cat, Entities: nil}
			key := signatureKey(sig)
			if _, ok := clusters[key]; !ok {
				clusters[key] = &clusterEntry{
					sig:     sig,
					markets: map[string]*models.CanonicalMarket{},
				}
				order = append(order, key)
			}
			clusters[key].markets[m.VenueMarketID] = m
			continue
		}

		// Assign to a cluster for EACH entity (multi-membership)
		for _, entity := range uniqueEntities {
			sig := TopicSignature{
				Category: cat,
				Entities: []string{entity},
			}
			key := signatureKey(sig)
			if _, ok := clusters[key]; !ok {
				clusters[key] = &clusterEntry{
					sig:     sig,
					markets: map[string]*models.CanonicalMarket{},
				}
				order = append(order, key)
			}
			clusters[key].markets[m.VenueMarketID] = m
		}

		// Also assign to the full entity combination cluster if >1 entity
		if len(uniqueEntities) > 1 {
			sig := TopicSignature{
				Category: cat,
				Entities: uniqueEntities,
			}
			key := signatureKey(sig)
			if _, ok := clusters[key]; !ok {
				clusters[key] = &clusterEntry{
					sig:     sig,
					markets: map[string]*models.CanonicalMarket{},
				}
				order = append(order, key)
			}
			clusters[key].markets[m.VenueMarketID] = m
		}
	}

	// Build result, skipping empty clusters
	var result []*TopicCluster
	for _, key := range order {
		entry := clusters[key]
		if len(entry.markets) == 0 {
			continue
		}
		tc := &TopicCluster{
			ID:        key,
			Label:     signatureLabel(entry.sig),
			Signature: entry.sig,
		}
		for _, m := range entry.markets {
			tc.Markets = append(tc.Markets, m)
		}
		result = append(result, tc)
	}

	// Sort: cross-venue clusters first (most useful), then by size
	sort.SliceStable(result, func(i, j int) bool {
		ci := result[i].HasCrossVenue()
		cj := result[j].HasCrossVenue()
		if ci != cj {
			return ci
		}
		return len(result[i].Markets) > len(result[j].Markets)
	})

	return result
}

// extractNormKeyEntities extracts important tokens from the normalized title
// that might not be caught by extractEntities (which looks for capitalized words).
// This catches synonyms like "btc" → "bitcoin" and abbreviations.
func extractNormKeyEntities(title string) []string {
	norm := normTitle(title)
	kw := keywords(norm)

	// Key entity keywords: tokens that are likely topic-defining
	// (proper nouns, crypto names, political figures after synonym expansion)
	var entities []string
	for w := range kw {
		if isTopicKeyword(w) {
			entities = append(entities, w)
		}
	}
	return entities
}

// topicKeywords are words that, after synonym expansion, strongly indicate a topic.
// These are checked against the normalized+synonym-expanded title.
var topicKeywords = map[string]bool{
	// Crypto
	"bitcoin": true, "ethereum": true, "solana": true, "dogecoin": true,
	"ripple": true, "cardano": true, "crypto": true,
	// Politics
	"trump": true, "biden": true, "harris": true, "obama": true,
	"desantis": true, "newsom": true, "vance": true, "pence": true,
	"democrat": true, "republican": true, "president": true,
	"supreme court": true,
	// Economics
	"federal reserve": true, "recession": true, "inflation": true,
	"gross domestic product": true, "consumer price index": true,
	"interest": true, "tariff": true,
	// Tech
	"tesla": true, "apple": true, "google": true, "nvidia": true,
	"openai": true, "spacex": true, "tiktok": true, "meta": true,
	// Sports
	"nba": true, "nfl": true, "mlb": true, "nhl": true,
	"super bowl": true, "world cup": true, "olympics": true,
	// Geopolitics
	"ukraine": true, "russia": true, "china": true, "taiwan": true,
	"nato": true, "israel": true, "iran": true,
}

func isTopicKeyword(w string) bool {
	return topicKeywords[w]
}
