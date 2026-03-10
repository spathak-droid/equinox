// Package matcher implements the equivalence detection pipeline.
//
// # What does "equivalent" mean?
//
// Two markets are considered equivalent when they represent the same real-world binary
// question and are expected to resolve on approximately the same date.
//
// This definition has three components:
//  1. Same question — the underlying event is the same (e.g. "Will X win election Y")
//  2. Same binary outcome — both YES outcomes refer to the same real-world result
//  3. Same resolution window — they resolve within MaxDateDeltaDays of each other
//
// # Detection methodology
//
// We use a four-stage pipeline:
//
//  Stage 1 — Hard filters (fast, cheap):
//    - Date proximity: resolution dates must be within MaxDateDeltaDays. Markets with no
//      resolution date are excluded from date-gated matching but may still match via
//      embedding similarity alone.
//    - Status: both markets must be active.
//
//  Stage 2 — Fuzzy title matching (fast, no API cost):
//    - Normalized edit distance (Levenshtein) + keyword Jaccard overlap
//    - Keyword Jaccard overlap after stopword removal
//    - Score: [0.0, 1.0]
//
//  Stage 3 — Embedding cosine similarity (slower, requires OpenAI key):
//    - Cosine similarity between pre-computed title embeddings
//    - Score: [0.0, 1.0]
//    - Skipped when embeddings are unavailable
//
//  Stage 4 — LLM pairwise disambiguation (only for ambiguous pairs):
//    - Applied to pairs where composite score is in [ProbableMatchThreshold, MatchThreshold)
//    - Batches all ambiguous pairs into a single OpenAI chat API call
//    - LLM returns match/no_match/unsure per pair
//    - match   → upgrades to ConfidenceMatch
//    - no_match → removes pair from results
//    - unsure  → keeps as ConfidenceProbable
//
// Final composite score:
//   if embeddings available:  0.40 * fuzzy + 0.60 * embedding
//   if embeddings absent:     1.00 * fuzzy
//
// Thresholds:
//   composite >= MatchThreshold        → MATCH (skip Stage 4)
//   composite >= ProbableMatchThreshold → sent to Stage 4 LLM
//   else                                → NO_MATCH
//
// # Known limitations
//   - Markets with different expiry but same question (e.g. monthly rolling contracts)
//     may be incorrectly merged if MaxDateDeltaDays is set too high.
//   - Short, generic titles (e.g. "Will inflation rise?") have high fuzzy similarity
//     even when they refer to different time periods. The date filter is the main
//     protection against these false positives.
package matcher

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

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
	FuzzyScore     float64
	EmbeddingScore float64 // -1 if not computed
	DatePenalty    float64 // 0.0 = no penalty, 1.0 = full penalty (dates too far apart)

	// Human-readable explanation of why this decision was made
	Explanation string
}

// Matcher finds equivalent markets across venues.
type Matcher struct {
	cfg    *config.Config
	openai *openai.Client // nil when no API key configured; enables Stage 4 LLM disambiguation
}

// New creates a Matcher with the given configuration.
// openaiClient may be nil; if set, Stage 4 LLM disambiguation is enabled for ambiguous pairs.
func New(cfg *config.Config, openaiClient *openai.Client) *Matcher {
	return &Matcher{cfg: cfg, openai: openaiClient}
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

	fmt.Printf("[matcher] Stages 1-3 complete: %d confirmed, %d ambiguous, %d rejected\n",
		len(confirmed), len(ambiguous), compared-len(confirmed)-len(ambiguous))

	// Stage 4: LLM disambiguation for ambiguous pairs
	if m.openai != nil && len(ambiguous) > 0 {
		resolved := m.disambiguateWithLLM(ctx, ambiguous)
		for _, r := range resolved {
			if r.Confidence != ConfidenceNoMatch {
				confirmed = append(confirmed, r)
			}
		}
	} else if len(ambiguous) > 0 {
		// No LLM available — only keep ambiguous pairs with high composite scores
		// to avoid flooding results with low-quality matches.
		fmt.Printf("[matcher] No LLM available — filtering %d ambiguous pairs (keeping composite >= %.2f)\n",
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

// compare runs the full four-stage pipeline for a single market pair
// (the final stage is conditionally executed by the caller during post-processing).
func (m *Matcher) compare(a, b *models.CanonicalMarket) *MatchResult {
	result := &MatchResult{
		MarketA:        a,
		MarketB:        b,
		EmbeddingScore: -1, // sentinel: not computed
	}

	// Stage 1: Hard filters
	if !m.passesHardFilters(a, b, result) {
		result.Confidence = ConfidenceNoMatch
		return result
	}

	// Stage 2: Fuzzy title match
	result.FuzzyScore = fuzzyTitleScore(a.Title, b.Title)

	// Stage 3: Embedding similarity (optional)
	if a.TitleEmbedding != nil && b.TitleEmbedding != nil {
		result.EmbeddingScore = cosineSimilarity(a.TitleEmbedding, b.TitleEmbedding)
	}

	// Composite score
	if result.EmbeddingScore >= 0 {
		result.CompositeScore = 0.40*result.FuzzyScore + 0.60*result.EmbeddingScore
	} else {
		result.CompositeScore = result.FuzzyScore
	}

	// Apply date penalty as a soft multiplier on the composite score.
	// This smoothly reduces scores for date-mismatched markets instead of
	// hard-rejecting them, so near-threshold pairs still get a chance.
	result.DatePenalty = m.datePenalty(a, b)
	if result.DatePenalty > 0 {
		result.CompositeScore *= (1.0 - result.DatePenalty)
	}

	// Classification
	dateSuffix := ""
	if result.DatePenalty > 0 {
		dateSuffix = fmt.Sprintf(", date_penalty=%.2f", result.DatePenalty)
	}
	switch {
	case result.CompositeScore >= m.cfg.MatchThreshold:
		result.Confidence = ConfidenceMatch
		result.Explanation = fmt.Sprintf(
			"High confidence match: fuzzy=%.2f, embedding=%.2f, composite=%.2f (threshold=%.2f)%s",
			result.FuzzyScore, result.EmbeddingScore, result.CompositeScore, m.cfg.MatchThreshold, dateSuffix)
	case result.CompositeScore >= m.cfg.ProbableMatchThreshold:
		result.Confidence = ConfidenceProbable
		result.Explanation = fmt.Sprintf(
			"Probable match — human review recommended: fuzzy=%.2f, embedding=%.2f, composite=%.2f%s",
			result.FuzzyScore, result.EmbeddingScore, result.CompositeScore, dateSuffix)
	default:
		result.Confidence = ConfidenceNoMatch
		result.Explanation = fmt.Sprintf(
			"No match: composite=%.2f below threshold=%.2f%s",
			result.CompositeScore, m.cfg.ProbableMatchThreshold, dateSuffix)
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
// resolution dates are. The penalty is:
//   - 0.0 when dates are within MaxDateDeltaDays (no penalty)
//   - Linear ramp from 0.0 to 1.0 between MaxDateDeltaDays and 2×MaxDateDeltaDays
//   - 1.0 (full penalty) beyond 2×MaxDateDeltaDays
//
// When either market lacks a resolution date, returns 0.0 (no penalty — we can't
// tell if dates are mismatched, so we let other signals decide).
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

// ─── Stage 4: LLM disambiguation ─────────────────────────────────────────────

// llmPairResult is the per-pair response from the LLM disambiguation call.
type llmPairResult struct {
	Index  int    `json:"index"`
	Result string `json:"result"` // "match", "no_match", or "unsure"
	Reason string `json:"reason"`
}

// disambiguateWithLLM sends ambiguous pairs to the OpenAI chat API in batches
// and upgrades/downgrades confidence based on the LLM's verdict.
//
// Pairs are sent in batches of 20 to stay within token limits.
// Any pair the LLM marks "match" is upgraded to ConfidenceMatch.
// Any pair marked "no_match" is downgraded to ConfidenceNoMatch (filtered out).
// "unsure" pairs remain as ConfidenceProbable.
func (m *Matcher) disambiguateWithLLM(ctx context.Context, pairs []*MatchResult) []*MatchResult {
	const batchSize = 20
	const maxLLMPairs = 40 // cap to avoid excessive API calls

	// Only send the top-scoring ambiguous pairs to the LLM
	toProcess := pairs
	if len(toProcess) > maxLLMPairs {
		fmt.Printf("[matcher] LLM disambiguation: capping from %d to %d ambiguous pairs\n",
			len(toProcess), maxLLMPairs)
		toProcess = toProcess[:maxLLMPairs]
	}

	fmt.Printf("[matcher] LLM disambiguation: %d ambiguous pairs in batches of %d\n",
		len(toProcess), batchSize)

	// Start with a copy; pairs beyond maxLLMPairs are dropped (not enough
	// confidence to include without LLM verification).
	results := make([]*MatchResult, len(toProcess))
	copy(results, toProcess)

	// Fire all LLM batches concurrently
	type batchResult struct {
		start    int
		end      int
		verdicts []llmPairResult
		err      error
	}

	var batches []batchResult
	for start := 0; start < len(toProcess); start += batchSize {
		end := start + batchSize
		if end > len(toProcess) {
			end = len(toProcess)
		}
		batches = append(batches, batchResult{start: start, end: end})
	}

	batchCh := make(chan batchResult, len(batches))
	for _, b := range batches {
		go func(start, end int) {
			batch := toProcess[start:end]
			fmt.Printf("[matcher] LLM batch %d-%d of %d...\n", start+1, end, len(toProcess))
			batchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			verdicts, err := m.llmBatchCall(batchCtx, batch)
			cancel()
			batchCh <- batchResult{start: start, end: end, verdicts: verdicts, err: err}
		}(b.start, b.end)
	}

	for range batches {
		b := <-batchCh
		if b.err != nil {
			fmt.Printf("[matcher] WARNING: LLM disambiguation failed (batch %d-%d): %v — keeping as PROBABLE_MATCH\n", b.start+1, b.end, b.err)
			continue
		}
		for _, v := range b.verdicts {
			idx := b.start + v.Index
			if idx >= len(results) {
				continue
			}
			switch v.Result {
			case "match":
				results[idx].Confidence = ConfidenceMatch
				results[idx].Explanation = fmt.Sprintf(
					"LLM confirmed match: %s (fuzzy=%.2f, embedding=%.2f, composite=%.2f)",
					v.Reason, results[idx].FuzzyScore, results[idx].EmbeddingScore, results[idx].CompositeScore)
			case "no_match":
				results[idx].Confidence = ConfidenceNoMatch
				results[idx].Explanation = fmt.Sprintf("LLM rejected: %s", v.Reason)
			default:
				results[idx].Explanation = fmt.Sprintf(
					"LLM unsure: %s (fuzzy=%.2f, embedding=%.2f, composite=%.2f)",
					v.Reason, results[idx].FuzzyScore, results[idx].EmbeddingScore, results[idx].CompositeScore)
			}
		}
	}

	return results
}

// llmBatchCall sends a single batch of pairs to the chat API and parses the response.
func (m *Matcher) llmBatchCall(ctx context.Context, pairs []*MatchResult) ([]llmPairResult, error) {
	// Build the pair list for the prompt
	var sb strings.Builder
	for i, p := range pairs {
		fmt.Fprintf(&sb, "%d. A: %q\n   B: %q\n", i, p.MarketA.Title, p.MarketB.Title)
	}

	prompt := fmt.Sprintf(`You are a prediction market equivalence classifier.

For each numbered pair below, determine if market A and market B are asking the SAME binary yes/no question about the same real-world event.

Rules:
- "match" = same underlying question, same outcome (different phrasing is fine)
- "no_match" = different questions or different outcomes
- "unsure" = genuinely ambiguous

Pairs:
%s
Respond with a JSON array only, no other text:
[{"index": 0, "result": "match"|"no_match"|"unsure", "reason": "brief reason"}, ...]`, sb.String())

	resp, err := m.openai.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: "gpt-4o-mini",
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("openai chat API: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai returned no choices")
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	// Strip markdown code fences if present
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var verdicts []llmPairResult
	if err := json.Unmarshal([]byte(content), &verdicts); err != nil {
		return nil, fmt.Errorf("parsing LLM response: %w\nraw: %s", err, content)
	}

	return verdicts, nil
}

// ─── Fuzzy title scoring ──────────────────────────────────────────────────────

// fuzzyTitleScore returns a [0.0, 1.0] similarity score for two market titles.
// It combines normalized edit distance with keyword Jaccard overlap, and applies
// an entity mismatch penalty when titles share a structural pattern but differ
// in the key entity (e.g., "Will X win Y" vs "Will Z win Y").
//
// The combined score = 0.5 * editSim + 0.5 * jaccardSim - entityPenalty
// This balances character-level similarity (good for variants like "U.S." vs "US")
// with semantic keyword overlap (good for paraphrased questions).
func fuzzyTitleScore(a, b string) float64 {
	na, nb := normTitle(a), normTitle(b)

	editSim := editSimilarity(na, nb)
	jaccardSim := keywordJaccard(na, nb)

	base := 0.5*editSim + 0.5*jaccardSim

	// Entity mismatch penalty: if titles are structurally similar (high Jaccard)
	// but differ in a key named entity, they're likely different markets about
	// different subjects (e.g., "Will Oprah win X" vs "Will LeBron win X").
	penalty := entityMismatchPenalty(na, nb, jaccardSim)

	score := base - penalty
	if score < 0 {
		score = 0
	}
	return score
}

// entityMismatchPenalty detects when two titles share a template but differ in
// the subject entity. Returns a penalty in [0.0, 0.3] to subtract from the score.
//
// Example: "oprah winfrey win the 2028 democratic presidential nomination"
//      vs  "hunter biden win the 2028 democratic presidential nomination"
// These share 80%+ keywords but are about different people — NOT equivalent markets.
func entityMismatchPenalty(a, b string, jaccardSim float64) float64 {
	// Only apply when titles are structurally similar (Jaccard > 0.5)
	// but not identical
	if jaccardSim < 0.5 || a == b {
		return 0
	}

	wordsA := strings.Fields(a)
	wordsB := strings.Fields(b)

	// Find words unique to each title (the "differing" parts)
	setA := map[string]bool{}
	setB := map[string]bool{}
	for _, w := range wordsA {
		setA[w] = true
	}
	for _, w := range wordsB {
		setB[w] = true
	}

	var onlyA, onlyB []string
	for w := range setA {
		if !setB[w] {
			onlyA = append(onlyA, w)
		}
	}
	for w := range setB {
		if !setA[w] {
			onlyB = append(onlyB, w)
		}
	}

	// If both titles have unique words (entity-like differences) and high overlap,
	// this is a template match with different subjects
	if len(onlyA) > 0 && len(onlyB) > 0 {
		// The more shared words relative to unique words, the more likely it's
		// a template mismatch (same question pattern, different entity)
		shared := 0
		for w := range setA {
			if setB[w] {
				shared++
			}
		}
		totalUnique := len(onlyA) + len(onlyB)
		if shared > 0 && totalUnique > 0 {
			templateRatio := float64(shared) / float64(shared+totalUnique)
			if templateRatio > 0.6 {
				// Strong template match with entity swap — heavy penalty
				return 0.25
			}
			if templateRatio > 0.4 {
				return 0.15
			}
		}
	}

	return 0
}

// normTitle lowercases and strips punctuation for comparison.
func normTitle(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, ch := range s {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == ' ' {
			b.WriteRune(ch)
		}
	}
	// Collapse multiple spaces
	return strings.Join(strings.Fields(b.String()), " ")
}

// editSimilarity returns 1 - normalizedLevenshtein(a, b).
func editSimilarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	d := levenshtein(a, b)
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 1.0
	}
	return 1.0 - float64(d)/float64(maxLen)
}

// keywordJaccard returns the Jaccard similarity of keyword sets after removing stopwords.
func keywordJaccard(a, b string) float64 {
	setA := keywords(a)
	setB := keywords(b)

	if len(setA) == 0 && len(setB) == 0 {
		return 1.0
	}

	intersection := 0
	for w := range setA {
		if setB[w] {
			intersection++
		}
	}

	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// keywords returns the set of meaningful words in a title, removing stopwords.
func keywords(s string) map[string]bool {
	stops := map[string]bool{
		"will": true, "the": true, "a": true, "an": true, "be": true,
		"is": true, "in": true, "on": true, "of": true, "to": true,
		"by": true, "at": true, "or": true, "and": true, "for": true,
		"win": true, "get": true, "have": true, "has": true, "its": true,
	}
	set := map[string]bool{}
	for _, w := range strings.Fields(s) {
		if !stops[w] && len(w) > 1 {
			set[w] = true
		}
	}
	return set
}

// levenshtein computes the Levenshtein edit distance between two strings.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)

	dp := make([][]int, la+1)
	for i := range dp {
		dp[i] = make([]int, lb+1)
	}
	for i := 0; i <= la; i++ {
		dp[i][0] = i
	}
	for j := 0; j <= lb; j++ {
		dp[0][j] = j
	}
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			if ra[i-1] == rb[j-1] {
				dp[i][j] = dp[i-1][j-1]
			} else {
				dp[i][j] = 1 + min3(dp[i-1][j], dp[i][j-1], dp[i-1][j-1])
			}
		}
	}
	return dp[la][lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// ─── Embedding cosine similarity ─────────────────────────────────────────────

// cosineSimilarity computes the cosine similarity between two float32 vectors.
// Returns a value in [-1.0, 1.0]; for well-formed embeddings, expect [0.0, 1.0].
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
