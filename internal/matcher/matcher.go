// Package matcher implements the equivalence detection pipeline.
//
// # What does "equivalent" mean?
//
// Two markets are considered equivalent when they represent the same real-world binary
// question and are expected to resolve on approximately the same date.
//
// # Detection methodology
//
// We use a multi-stage pipeline (no AI/LLM/embeddings):
//
//  Stage 0 — Signature exact-match (instant, free):
//    - Canonical semantic signature hash comparison.
//    - If both markets hash identically, they're asking the same question.
//
//  Stage 1 — Hard filters (fast, cheap):
//    - Status: both markets must be active.
//
//  Stage 1b — Semantic gate:
//    - If both signatures are well-populated but incompatible (different entities,
//      thresholds, or actions), reject immediately.
//
//  Stage 2 — Fuzzy title matching (fast, no API cost):
//    - Normalized edit distance (Levenshtein) + keyword Jaccard overlap
//    - Score: [0.0, 1.0]
//
//  Stage 3 — Multi-signal composite scoring:
//    - Event signature match, entity overlap, date proximity, price proximity, category bonus
//
// Final composite score (rule-based):
//
//	composite = 0.30*fuzzy + 0.25*eventMatch + 0.15*entityOverlap +
//	            0.12*dateProximity + 0.08*priceProximity + 0.10*categoryBonus
//
// Thresholds:
//
//	composite >= MatchThreshold         → MATCH
//	composite >= ProbableMatchThreshold → PROBABLE_MATCH
//	else                                → NO_MATCH
package matcher

import (
	"context"
	"fmt"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
)

// MatchConfidence describes how certain the matcher is about an equivalence decision.
type MatchConfidence string

const (
	ConfidenceMatch        MatchConfidence = "MATCH"
	ConfidenceProbable     MatchConfidence = "PROBABLE_MATCH"
	ConfidenceNoMatch      MatchConfidence = "NO_MATCH"
)

// MatchResult describes the outcome of comparing two markets.
type MatchResult struct {
	MarketA        *models.CanonicalMarket
	MarketB        *models.CanonicalMarket
	Confidence     MatchConfidence
	CompositeScore float64

	// Component scores for transparency
	FuzzyScore          float64
	EmbeddingScore      float64 // always -1 (embeddings removed)
	EventMatchScore     float64 // structured event signature match [0.0, 1.0]
	EntityOverlapScore  float64 // -1 if not computed
	DateProximityScore  float64 // -1 if not computed
	PriceProximityScore float64 // -1 if not computed
	DatePenalty         float64 // 0.0 = no penalty, 1.0 = full penalty (dates too far apart)
	SignatureMatch      bool    // true when Stage 0 signature exact-match was used

	// Human-readable explanation of why this decision was made
	Explanation string
}

// Matcher finds equivalent markets across venues.
type Matcher struct {
	cfg *config.Config
}

// New creates a Matcher with the given configuration.
func New(cfg *config.Config) *Matcher {
	return &Matcher{cfg: cfg}
}

// FindEquivalentPairs compares all markets from different venues and returns
// a list of matched pairs sorted by composite score descending.
//
// Only cross-venue pairs are considered — we never compare a market to itself
// or to another market from the same venue.
func (m *Matcher) FindEquivalentPairs(ctx context.Context, markets []*models.CanonicalMarket) []*MatchResult {
	var confirmed []*MatchResult
	var ambiguous []*MatchResult

	// Count cross-venue pairs for progress
	crossVenue := 0
	for i := 0; i < len(markets); i++ {
		for j := i + 1; j < len(markets); j++ {
			if markets[i].VenueID != markets[j].VenueID {
				crossVenue++
			}
		}
	}
	fmt.Printf("[matcher] Comparing %d cross-venue pairs...\n", crossVenue)

	compared := 0
	for i := 0; i < len(markets); i++ {
		for j := i + 1; j < len(markets); j++ {
			a, b := markets[i], markets[j]

			if a.VenueID == b.VenueID {
				continue
			}

			result := m.compare(a, b)
			compared++
			switch result.Confidence {
			case ConfidenceMatch:
				confirmed = append(confirmed, result)
			case ConfidenceProbable:
				ambiguous = append(ambiguous, result)
			}
		}
	}

	fmt.Printf("[matcher] Comparison complete: %d confirmed, %d ambiguous, %d rejected\n",
		len(confirmed), len(ambiguous), compared-len(confirmed)-len(ambiguous))

	// Keep ambiguous pairs with high composite scores
	if len(ambiguous) > 0 {
		fmt.Printf("[matcher] Filtering %d ambiguous pairs (keeping composite >= %.2f)\n",
			len(ambiguous), m.cfg.MatchThreshold)
		for _, r := range ambiguous {
			if r.CompositeScore >= m.cfg.MatchThreshold {
				confirmed = append(confirmed, r)
			}
		}
	}

	// Log final results
	for _, r := range confirmed {
		fmt.Printf("[matcher] %s — %s vs %s (score=%.3f): %s\n",
			r.Confidence, r.MarketA.VenueID, r.MarketB.VenueID, r.CompositeScore, r.Explanation)
	}

	// Sort by composite score descending
	for i := 1; i < len(confirmed); i++ {
		for j := i; j > 0 && confirmed[j].CompositeScore > confirmed[j-1].CompositeScore; j-- {
			confirmed[j], confirmed[j-1] = confirmed[j-1], confirmed[j]
		}
	}

	return confirmed
}

// TopRejectedPairs returns the highest-scoring cross-venue pairs that were rejected.
// Useful for debugging why no final matches were produced.
func (m *Matcher) TopRejectedPairs(markets []*models.CanonicalMarket, limit int) []*MatchResult {
	if limit <= 0 {
		return nil
	}
	var rejected []*MatchResult
	for i := 0; i < len(markets); i++ {
		for j := i + 1; j < len(markets); j++ {
			a, b := markets[i], markets[j]
			if a.VenueID == b.VenueID {
				continue
			}
			r := m.compare(a, b)
			if r.Confidence == ConfidenceNoMatch {
				rejected = append(rejected, r)
			}
		}
	}
	for i := 1; i < len(rejected); i++ {
		for j := i; j > 0 && rejected[j].CompositeScore > rejected[j-1].CompositeScore; j-- {
			rejected[j], rejected[j-1] = rejected[j-1], rejected[j]
		}
	}
	if len(rejected) > limit {
		rejected = rejected[:limit]
	}
	return rejected
}

// compare runs the full pipeline for a single market pair.
// Stage 0: Signature exact-match (instant, free)
// Stage 1: Hard filters + semantic gates
// Stage 2: Fuzzy title match
// Stage 3: Multi-signal composite scoring
func (m *Matcher) compare(a, b *models.CanonicalMarket) *MatchResult {
	result := &MatchResult{
		MarketA:             a,
		MarketB:             b,
		EmbeddingScore:      -1, // embeddings removed
		EntityOverlapScore:  -1,
		DateProximityScore:  -1,
		PriceProximityScore: -1,
	}

	// Stage 0: Signature exact-match
	sigA := ExtractEventSignature(a.Title)
	sigB := ExtractEventSignature(b.Title)

	// Populate semantic signatures on the canonical markets for downstream use
	if csA := sigA.CanonicalSignature(); csA != "" {
		a.SemanticSignature = csA
	}
	if csB := sigB.CanonicalSignature(); csB != "" {
		b.SemanticSignature = csB
	}

	if a.SemanticSignature != "" && b.SemanticSignature != "" &&
		a.SemanticSignature == b.SemanticSignature {
		result.SignatureMatch = true
		result.FuzzyScore = fuzzyTitleScore(a.Title, b.Title)
		result.EventMatchScore = 1.0

		// Still apply date penalty — same semantic signature but wildly different
		// resolution dates (e.g. "before 2027" vs "before 2030") should not match.
		result.DatePenalty = m.datePenalty(a, b)
		result.CompositeScore = 1.0 * (1.0 - result.DatePenalty)

		dateSuffix := ""
		if result.DatePenalty > 0 {
			dateSuffix = fmt.Sprintf(", date_penalty=%.2f", result.DatePenalty)
		}

		if result.CompositeScore >= m.cfg.MatchThreshold {
			result.Confidence = ConfidenceMatch
		} else if result.CompositeScore >= m.cfg.ProbableMatchThreshold {
			result.Confidence = ConfidenceProbable
		} else {
			result.Confidence = ConfidenceNoMatch
		}

		result.Explanation = fmt.Sprintf(
			"Stage 0 signature match: sig=%s (entities=%v, threshold=%s, comparator=%s, date=%s)%s",
			a.SemanticSignature, sigA.Entities, sigA.Threshold, sigA.Comparator, sigA.DateRef, dateSuffix)
		return result
	}

	// Stage 1: Hard filters + semantic compatibility gates
	if !m.passesHardFilters(a, b, result) {
		result.Confidence = ConfidenceNoMatch
		return result
	}

	// Semantic gate: if both signatures are well-populated but incompatible,
	// reject early.
	if sigA.Confidence >= 0.25 && sigB.Confidence >= 0.25 {
		if !SignaturesCompatible(sigA, sigB) {
			result.Confidence = ConfidenceNoMatch
			result.EventMatchScore = EventMatchScore(sigA, sigB)
			result.Explanation = fmt.Sprintf(
				"Semantic gate rejection: signatures incompatible (A: entities=%v threshold=%s comp=%s, B: entities=%v threshold=%s comp=%s)",
				sigA.Entities, sigA.Threshold, sigA.Comparator,
				sigB.Entities, sigB.Threshold, sigB.Comparator)
			return result
		}
	}

	// Semantic gate: reject template matches with different specific entities
	// (e.g., "Spain wins World Cup" vs "Iran to compete in World Cup").
	if disjointSpecificEntities(a.Title, b.Title) {
		result.Confidence = ConfidenceNoMatch
		result.EventMatchScore = EventMatchScore(sigA, sigB)
		result.Explanation = "Semantic gate rejection: specific entities differ with no overlap"
		return result
	}

	// Stage 2: Fuzzy title match
	result.FuzzyScore = fuzzyTitleScore(a.Title, b.Title)

	// Event signature scoring
	result.EventMatchScore = EventMatchScore(sigA, sigB)

	// Stage 3: Multi-signal composite scoring (no embeddings)
	result.EntityOverlapScore = entityOverlapScore(a.Title, b.Title)
	result.DateProximityScore = dateProximityScore(a, b, m.cfg.MaxDateDeltaDays)
	result.PriceProximityScore = priceProximityScore(a, b)
	catBonus := categoryBonus(a, b)

	result.CompositeScore = 0.30*result.FuzzyScore +
		0.25*result.EventMatchScore +
		0.15*result.EntityOverlapScore +
		0.12*result.DateProximityScore +
		0.08*result.PriceProximityScore +
		0.10*catBonus

	// Hard veto: if both titles have thresholds and they're different,
	// these are different events regardless of other signals.
	if sigA.Threshold != "" && sigB.Threshold != "" && sigA.Threshold != sigB.Threshold {
		result.CompositeScore *= 0.5 // heavy penalty for threshold mismatch
	}

	// Apply date penalty as a soft multiplier on the composite score.
	result.DatePenalty = m.datePenalty(a, b)
	if result.DatePenalty > 0 {
		result.CompositeScore *= (1.0 - result.DatePenalty)
	}

	// Guard 1: Template mismatch — both titles have named entities but they
	// don't overlap (different subjects in the same event/race).
	if result.EntityOverlapScore >= 0 && result.EntityOverlapScore < 0.40 {
		entA := extractEntities(a.Title)
		entB := extractEntities(b.Title)
		if len(entA) > 0 && len(entB) > 0 {
			mismatchPenalty := 0.40 - result.EntityOverlapScore
			result.CompositeScore *= (1.0 - mismatchPenalty)
		}
	}

	// Guard 2: Low title similarity floor — when fuzzy score is very low,
	// entity/date/price signals alone should not produce a match.
	if result.FuzzyScore < 0.35 {
		if result.CompositeScore > m.cfg.ProbableMatchThreshold {
			result.CompositeScore = m.cfg.ProbableMatchThreshold
		}
	}

	// Classification
	dateSuffix := ""
	if result.DatePenalty > 0 {
		dateSuffix = fmt.Sprintf(", date_penalty=%.2f", result.DatePenalty)
	}
	sigSuffix := ""
	if sigA.Confidence > 0 || sigB.Confidence > 0 {
		sigSuffix = fmt.Sprintf(", sig_conf=%.0f%%/%.0f%%", sigA.Confidence*100, sigB.Confidence*100)
	}
	switch {
	case result.CompositeScore >= m.cfg.MatchThreshold:
		result.Confidence = ConfidenceMatch
		result.Explanation = fmt.Sprintf(
			"High confidence match: fuzzy=%.2f, event=%.2f, entity=%.2f, date=%.2f, price=%.2f, composite=%.2f (threshold=%.2f)%s%s",
			result.FuzzyScore, result.EventMatchScore, result.EntityOverlapScore,
			result.DateProximityScore, result.PriceProximityScore,
			result.CompositeScore, m.cfg.MatchThreshold, dateSuffix, sigSuffix)
	case result.CompositeScore >= m.cfg.ProbableMatchThreshold:
		result.Confidence = ConfidenceProbable
		result.Explanation = fmt.Sprintf(
			"Probable match: fuzzy=%.2f, event=%.2f, entity=%.2f, date=%.2f, price=%.2f, composite=%.2f%s%s",
			result.FuzzyScore, result.EventMatchScore, result.EntityOverlapScore,
			result.DateProximityScore, result.PriceProximityScore, result.CompositeScore, dateSuffix, sigSuffix)
	default:
		result.Confidence = ConfidenceNoMatch
		result.Explanation = fmt.Sprintf(
			"No match: composite=%.2f below threshold=%.2f%s%s",
			result.CompositeScore, m.cfg.ProbableMatchThreshold, dateSuffix, sigSuffix)
	}

	return result
}

// passesHardFilters checks non-negotiable conditions (status).
// Returns false and populates result.Explanation on failure.
func (m *Matcher) passesHardFilters(a, b *models.CanonicalMarket, result *MatchResult) bool {
	// Both must be active
	if a.Status != models.StatusActive || b.Status != models.StatusActive {
		result.Explanation = "skipped: one or both markets are not active"
		return false
	}

	return true
}

// datePenalty returns a [0.0, 1.0] penalty based on how far apart two markets'
// resolution dates are.
func (m *Matcher) datePenalty(a, b *models.CanonicalMarket) float64 {
	if !a.HasResolutionDate() || !b.HasResolutionDate() {
		return 0
	}

	delta := a.ResolutionDate.Sub(*b.ResolutionDate)
	if delta < 0 {
		delta = -delta
	}
	deltaDays := delta.Hours() / 24
	maxDays := float64(m.cfg.MaxDateDeltaDays)

	if deltaDays <= maxDays {
		return 0
	}
	if deltaDays >= maxDays*2 {
		return 1.0
	}
	// Linear ramp in the buffer zone
	return (deltaDays - maxDays) / maxDays
}
