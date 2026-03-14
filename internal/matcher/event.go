// Package matcher — event.go implements structured event extraction from market titles.
//
// This separates "same event" from "similar topic" by extracting:
//   - Entity: who/what the market is about (e.g., "bitcoin", "trump")
//   - Action: what is being predicted (e.g., "reach", "win", "cut")
//   - Threshold: numeric target (e.g., "100000", "5")
//   - Direction: above/below/at semantic (e.g., "above", "under")
//   - DateRef: any date reference in the title (e.g., "2026", "june")
//   - Metric: what dimension is being measured (e.g., "price", "rate", "votes")
//   - Comparator: the comparison semantics (e.g., "reach", "exceed", "stay_above")
//   - MarketType: structural type (e.g., "binary", "range")
//   - Region: geographic scope when detectable (e.g., "us", "global")
//
// Two markets are the "same event" when their canonical signatures match.
// Two markets are "similar topic" when they share entities but differ on
// thresholds, dates, or direction.
//
// Extraction helpers live in event_extract.go.
// Compatibility checks and entity normalization live in event_compat.go.
package matcher

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// EventSignature is a structured representation of what a market is asking.
// Extracted purely from the normalized title — no AI needed.
type EventSignature struct {
	Entities  []string // key subjects (e.g., ["bitcoin"], ["trump"])
	Action    string   // predicted action (e.g., "reach", "win", "cut")
	Threshold string   // numeric target, normalized (e.g., "100000", "5")
	Direction string   // "above", "below", "at", "" (unknown)
	DateRef   string   // date reference from title (e.g., "2026", "june 2026")

	// Semantic fields added for deterministic matching
	Metric     string  // what's being measured: "price", "rate", "votes", "market_cap", "count", ""
	Comparator string  // normalized comparison: "reach", "exceed", "drop_below", "stay_above", "win", ""
	MarketType string  // "binary", "range", "multi" — defaults to "binary"
	Region     string  // geographic scope: "us", "eu", "global", "" (unknown)
	Confidence float64 // parse confidence [0.0, 1.0] — how many fields were extracted
}

// Key returns a deterministic string key for comparing event signatures.
// Two markets with the same Key() are asking the same question.
func (e *EventSignature) Key() string {
	entStr := strings.Join(e.Entities, "+")
	return fmt.Sprintf("%s|%s|%s|%s|%s", entStr, e.Action, e.Threshold, e.Direction, e.DateRef)
}

// CanonicalSignature returns a SHA-256 hash of the normalized semantic fields.
// Two markets with the same CanonicalSignature are asking the same question
// and can be matched instantly without scoring.
//
// Returns "" if insufficient fields are populated for a reliable signature
// (we require at least entity + one of: threshold, action, or metric).
func (e *EventSignature) CanonicalSignature() string {
	if len(e.Entities) == 0 {
		return "" // no entity = no reliable signature
	}
	// Require at least one discriminating field beyond entity
	if e.Threshold == "" && e.Action == "" && e.Metric == "" {
		return ""
	}

	// Sort entities for determinism
	sorted := make([]string, len(e.Entities))
	copy(sorted, e.Entities)
	sort.Strings(sorted)

	// Build canonical form: entity|metric|comparator|threshold|direction|dateref
	parts := []string{
		strings.Join(sorted, "+"),
		e.Metric,
		e.Comparator,
		e.Threshold,
		e.Direction,
		e.DateRef,
	}
	raw := strings.Join(parts, "|")

	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16]) // 128-bit is sufficient for dedup
}

// FilledFieldCount returns how many semantic fields were successfully extracted.
func (e *EventSignature) FilledFieldCount() int {
	count := 0
	if len(e.Entities) > 0 {
		count++
	}
	if e.Action != "" {
		count++
	}
	if e.Threshold != "" {
		count++
	}
	if e.Direction != "" {
		count++
	}
	if e.DateRef != "" {
		count++
	}
	if e.Metric != "" {
		count++
	}
	if e.Comparator != "" {
		count++
	}
	if e.Region != "" {
		count++
	}
	return count
}

// ExtractEventSignature parses a market title into structured fields.
// Works on the raw title (not pre-normalized) to preserve original phrasing.
func ExtractEventSignature(title string) *EventSignature {
	norm := strings.ToLower(strings.TrimSpace(title))

	sig := &EventSignature{
		MarketType: "binary", // default
	}

	// Extract entities (proper nouns from original title + topic keywords from normalized)
	rawEntities := extractEntities(title)
	normEntities := extractNormKeyEntities(title)
	entSet := map[string]bool{}
	for _, e := range rawEntities {
		e = strings.ToLower(e)
		if !entSet[e] {
			entSet[e] = true
			sig.Entities = append(sig.Entities, e)
		}
	}
	for _, e := range normEntities {
		e = strings.ToLower(e)
		if !entSet[e] {
			entSet[e] = true
			sig.Entities = append(sig.Entities, e)
		}
	}

	// Extract action verb
	sig.Action = extractAction(norm)

	// Extract direction
	sig.Direction = extractDirection(norm)

	// Extract threshold numbers from normalized title
	sig.Threshold = extractThreshold(norm)

	// Extract date references
	sig.DateRef = extractDateRef(norm)

	// Extract metric — what dimension is being measured
	sig.Metric = extractMetric(norm)

	// Extract comparator — normalized comparison semantics
	sig.Comparator = extractComparator(norm, sig.Action, sig.Direction)

	// Extract region — geographic scope
	sig.Region = extractRegion(norm)

	// Detect market type from title patterns
	sig.MarketType = detectMarketType(norm)

	// Normalize entities through synonym resolution so "fed" and "reserve"
	// both resolve to "federal reserve", and "btc" resolves to "bitcoin", etc.
	sig.Entities = normalizeEntities(sig.Entities)

	// Compute parse confidence based on how many fields we extracted
	total := 8.0 // entity, action, threshold, direction, dateref, metric, comparator, region
	sig.Confidence = float64(sig.FilledFieldCount()) / total

	return sig
}

// EventMatchScore compares two event signatures and returns a score in [0.0, 1.0].
// 1.0 = same event, 0.0 = completely different.
//
// This is used as an additional signal in the composite score to separate
// "same event" from "similar topic".
func EventMatchScore(a, b *EventSignature) float64 {
	score := 0.0
	weights := 0.0

	// Entity overlap (most important)
	if len(a.Entities) > 0 || len(b.Entities) > 0 {
		setA := map[string]bool{}
		for _, e := range a.Entities {
			setA[e] = true
		}
		inter := 0
		for _, e := range b.Entities {
			if setA[e] {
				inter++
			}
		}
		union := len(setA)
		for _, e := range b.Entities {
			if !setA[e] {
				union++
			}
		}
		if union > 0 {
			score += 0.30 * float64(inter) / float64(union)
		}
		weights += 0.30
	}

	// Threshold match (critical — $100k vs $150k is a different event)
	if a.Threshold != "" && b.Threshold != "" {
		if a.Threshold == b.Threshold {
			score += 0.30
		} else {
			// Different thresholds = definitely different events, even if everything else matches.
			// Heavy penalty: subtract from score AND add weight so it drags the normalized score down.
			score -= 0.30
		}
		weights += 0.30
	} else if a.Threshold == "" && b.Threshold == "" {
		// Both have no threshold — neutral
		score += 0.15
		weights += 0.30
	}

	// Direction match
	if a.Direction != "" && b.Direction != "" {
		if a.Direction == b.Direction {
			score += 0.10
		} else {
			score -= 0.05 // opposite direction = different question
		}
		weights += 0.10
	}

	// Action match — different actions on the same entity = different question
	if a.Action != "" && b.Action != "" {
		if a.Action == b.Action {
			score += 0.15
		} else {
			// Different action = likely different question (e.g. "win" vs "impeach")
			score -= 0.10
		}
		weights += 0.15
	}

	// Date reference match
	if a.DateRef != "" && b.DateRef != "" {
		if a.DateRef == b.DateRef {
			score += 0.15
		} else {
			score -= 0.05 // different date ref = different event
		}
		weights += 0.15
	}

	if weights == 0 {
		return 0.5 // no signal at all
	}

	// Normalize to [0, 1]
	result := score / weights
	if result < 0 {
		result = 0
	}
	if result > 1 {
		result = 1
	}
	return result
}
