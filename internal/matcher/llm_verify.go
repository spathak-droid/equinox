// Package matcher — llm_verify.go implements LLM-based pairwise market verification.
//
// When Qdrant finds semantically similar markets from different venues, we need to
// verify whether they are asking the EXACT same question. Rule-based scoring fails
// on subtle distinctions like "win the election" vs "be the nominee" — both share
// entities and keywords but are fundamentally different predictions.
//
// This uses a single batched LLM call to verify multiple candidate pairs at once,
// keeping latency to ~1-2 seconds for a typical search.
package matcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
)

// llmHTTPClient is used for LLM verification requests with a bounded timeout.
var llmHTTPClient = &http.Client{Timeout: 60 * time.Second}

// LLMVerifiedPair is a market pair confirmed as equivalent by the LLM.
type LLMVerifiedPair struct {
	MarketA *models.CanonicalMarket
	MarketB *models.CanonicalMarket
	Reason  string
}

// llmPairVerdict is the JSON structure returned by the LLM.
type llmPairVerdict struct {
	Pair   int    `json:"pair"`   // 0-based index into the candidate list
	Match  bool   `json:"match"`  // true if the markets ask the same question
	Reason string `json:"reason"` // brief explanation
}

// VerifyPairsWithLLM takes candidate cross-venue pairs and uses an LLM to determine
// which ones are asking the EXACT same question. Returns only confirmed matches.
//
// Each candidate is a pair of (source market, candidate market) from different venues.
// The LLM sees both titles and must decide if they resolve to the same outcome.
func VerifyPairsWithLLM(ctx context.Context, cfg *config.Config, candidates []SearchCandidate) ([]LLMVerifiedPair, error) {
	if cfg.OpenAIAPIKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Cap at 40 pairs per LLM call to avoid token limits
	if len(candidates) > 40 {
		candidates = candidates[:40]
	}

	// Build the prompt — kept generic, no venue-specific hardcoding.
	// Title normalization happens in cleanTitleForLLM() before the LLM sees them.
	var sb strings.Builder
	sb.WriteString("You are verifying whether prediction market questions from two different venues are asking the EXACT SAME question.\n\n")
	sb.WriteString("Two markets match ONLY IF a YES resolution on one guarantees a YES resolution on the other, and vice versa.\n\n")
	sb.WriteString("NO MATCH when:\n")
	sb.WriteString("- Different actions: 'win' ≠ 'play in', 'win' ≠ 'host', 'impeached' ≠ 'removed from office'\n")
	sb.WriteString("- Different thresholds: '$100K' ≠ '$150K', 'above 50' ≠ 'above 60'\n")
	sb.WriteString("- Different timeframes IN THE TITLE: 'by 2026' ≠ 'by 2028'\n")
	sb.WriteString("- Different subjects: 'Person A wins' ≠ 'Person B wins'\n")
	sb.WriteString("- Different scope: 'nominated' ≠ 'elected' (nomination doesn't guarantee election)\n\n")
	sb.WriteString("MATCH when:\n")
	sb.WriteString("- Same subject, same action/outcome, same timeframe\n")
	sb.WriteString("- Minor phrasing differences: 'Will X win?' ≈ 'X to win' ≈ 'Is X going to win?'\n")
	sb.WriteString("- Equivalent terms: 'Finals' ≈ 'Championship' for same sport and year\n")
	sb.WriteString("- City names match their teams in context (e.g. 'Philadelphia' = 'the 76ers' when discussing NBA)\n\n")
	sb.WriteString("IMPORTANT: Ignore the [closes ...] dates — those are venue settlement windows, NOT the event date.\n")
	sb.WriteString("Only compare dates/years that appear IN THE TITLE.\n\n")
	sb.WriteString("PAIRS TO VERIFY:\n")

	for i, c := range candidates {
		titleA := cleanTitleForLLM(c.Source.Title)
		titleB := cleanTitleForLLM(c.Candidate.Title)
		venueA := string(c.Source.VenueID)
		venueB := string(c.Candidate.VenueID)
		dateA, dateB := "", ""
		if c.Source.ResolutionDate != nil {
			dateA = fmt.Sprintf(" [closes %s]", c.Source.ResolutionDate.Format("2006-01-02"))
		}
		if c.Candidate.ResolutionDate != nil {
			dateB = fmt.Sprintf(" [closes %s]", c.Candidate.ResolutionDate.Format("2006-01-02"))
		}
		sb.WriteString(fmt.Sprintf("Pair %d:\n  A (%s): %q%s\n  B (%s): %q%s\n\n", i, venueA, titleA, dateA, venueB, titleB, dateB))
	}

	sb.WriteString("Return ONLY a JSON array. For each pair, include:\n")
	sb.WriteString(`- "pair": the pair number (0-based)` + "\n")
	sb.WriteString(`- "match": true if they ask the EXACT same question, false otherwise` + "\n")
	sb.WriteString(`- "reason": brief explanation (10 words max)` + "\n\n")
	sb.WriteString("Only include pairs where match=true. If no pairs match, return []\n")
	sb.WriteString("Return ONLY the JSON array, no other text.\n")

	prompt := sb.String()

	// Call OpenAI
	// Use gpt-4o for verification — accuracy matters more than speed/cost here.
	// gpt-4o-mini makes too many false positive errors on semantic equivalence.
	verifyModel := "gpt-4o"
	if cfg.OpenAIModel == "gpt-4o" || cfg.OpenAIModel == "gpt-4-turbo" {
		verifyModel = cfg.OpenAIModel
	}

	reqBody := map[string]interface{}{
		"model": verifyModel,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.0,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)

	resp, err := llmHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling OpenAI: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("OpenAI returned %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	content := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	fmt.Printf("[matcher/llm] LLM verification response: %s\n", content)

	var verdicts []llmPairVerdict
	if err := json.Unmarshal([]byte(content), &verdicts); err != nil {
		return nil, fmt.Errorf("parsing LLM verdicts: %w (content: %s)", err, content)
	}

	// Collect confirmed pairs
	var confirmed []LLMVerifiedPair
	for _, v := range verdicts {
		if !v.Match {
			continue
		}
		if v.Pair < 0 || v.Pair >= len(candidates) {
			continue
		}
		c := candidates[v.Pair]
		confirmed = append(confirmed, LLMVerifiedPair{
			MarketA: c.Source,
			MarketB: c.Candidate,
			Reason:  v.Reason,
		})
	}

	fmt.Printf("[matcher/llm] Verified %d/%d candidate pairs as true matches (model=%s)\n", len(confirmed), len(candidates), verifyModel)
	for _, v := range confirmed {
		fmt.Printf("[matcher/llm]   ✓ %q ≈ %q — %s\n", v.MarketA.Title, v.MarketB.Title, v.Reason)
	}
	return confirmed, nil
}

// cleanTitleForLLM performs generic title cleanup so the LLM sees clean,
// comparable titles. Venue-specific normalizations (e.g. league name replacements)
// belong in the normalizer package, not here.
func cleanTitleForLLM(title string) string {
	// Remove redundant "the" before country/team names: "Will the Brazil" → "Will Brazil"
	title = strings.ReplaceAll(title, "the the ", "the ")
	if strings.Contains(title, "Will the ") {
		// Only replace "Will the X" patterns, not "Will the market..."
		// Simple heuristic: if next word is capitalized, it's likely a team name
		parts := strings.SplitN(title, "Will the ", 2)
		if len(parts) == 2 && len(parts[1]) > 0 && parts[1][0] >= 'A' && parts[1][0] <= 'Z' {
			title = parts[0] + "Will " + parts[1]
		}
	}

	return title
}

// BuildCrossVenueCandidates creates candidate pairs from two sets of markets.
// Each market from venue A is paired with each market from venue B.
// Skips same-venue pairs and garbage titles.
func BuildCrossVenueCandidates(marketsA, marketsB []*models.CanonicalMarket) []SearchCandidate {
	var candidates []SearchCandidate
	for _, a := range marketsA {
		for _, b := range marketsB {
			if a.VenueID == b.VenueID {
				continue
			}
			candidates = append(candidates, SearchCandidate{
				Source:    a,
				Candidate: b,
			})
		}
	}
	return candidates
}

// RankCandidatesByBestMatch finds the best cross-venue match for each source market.
// Uses a combined score of entity overlap (weighted 60%) and fuzzy title similarity (40%)
// to handle template-style markets where surrounding text is identical but the subject differs
// (e.g., "Will [TEAM] win the 2026 NBA Finals?" — fuzzy alone can't distinguish teams).
// Returns at most topN pairs, deduplicated so each market appears at most once.
func RankCandidatesByBestMatch(marketsA, marketsB []*models.CanonicalMarket, topN int) []SearchCandidate {
	if len(marketsA) == 0 || len(marketsB) == 0 {
		return nil
	}

	type scored struct {
		source    *models.CanonicalMarket
		candidate *models.CanonicalMarket
		score     float64
	}

	// For each A market, find best B match using entity-weighted scoring
	var bestPairs []scored
	for _, a := range marketsA {
		var best *models.CanonicalMarket
		bestScore := -1.0
		for _, b := range marketsB {
			if a.VenueID == b.VenueID {
				continue
			}
			fuzzy := fuzzyTitleScore(a.Title, b.Title)
			entity := entityOverlapScore(a.Title, b.Title)
			if entity < 0 {
				entity = 0
			}
			// Entity overlap is the primary signal for template markets
			combined := 0.40*fuzzy + 0.60*entity
			if combined > bestScore {
				bestScore = combined
				best = b
			}
		}
		if best != nil && bestScore > 0.15 {
			bestPairs = append(bestPairs, scored{source: a, candidate: best, score: bestScore})
		}
	}

	// Sort by score descending
	for i := 1; i < len(bestPairs); i++ {
		for j := i; j > 0 && bestPairs[j].score > bestPairs[j-1].score; j-- {
			bestPairs[j], bestPairs[j-1] = bestPairs[j-1], bestPairs[j]
		}
	}

	// Deduplicate: each candidate market appears at most once
	usedB := map[string]bool{}
	var result []SearchCandidate
	for _, p := range bestPairs {
		key := string(p.candidate.VenueID) + ":" + p.candidate.VenueMarketID
		if usedB[key] {
			continue
		}
		usedB[key] = true
		result = append(result, SearchCandidate{Source: p.source, Candidate: p.candidate})
		if len(result) >= topN {
			break
		}
	}
	return result
}
