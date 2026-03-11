// Package matcher — llm.go implements LLM-based market matching using OpenAI.
//
// For each Polymarket market, we send it with all Kalshi candidates in a single
// LLM call and ask the model to score each candidate. This means N Polymarket
// markets = N API calls (all parallel), not N×M.
package matcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/equinox/internal/models"
)

// LLMMatchResult holds the result of an LLM comparison between two markets.
type LLMMatchResult struct {
	MarketA    *models.CanonicalMarket
	MarketB    *models.CanonicalMarket
	Confidence float64 // 0.0 to 1.0
	Reasoning  string
}

type LLMDecision string

const (
	LLMDecisionMatch   LLMDecision = "match"
	LLMDecisionNoMatch LLMDecision = "no_match"
	LLMDecisionUnsure  LLMDecision = "unsure"
)

// LLMJudgment is the raw model decision for one source/candidate pair.
type LLMJudgment struct {
	Decision   LLMDecision
	Confidence float64
	Reasoning  string
}

// LLMMatcher uses OpenAI to match markets across venues.
type LLMMatcher struct {
	apiKey     string
	httpClient *http.Client
	model      string
	minConf    float64
}

// NewLLMMatcher creates an LLM matcher. Returns nil if OPENAI_API_KEY is not set.
func NewLLMMatcher() *LLMMatcher {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil
	}
	model := os.Getenv("OPEN_AI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	minConf := 0.80
	if raw := os.Getenv("LLM_MIN_CONFIDENCE"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 && v <= 1 {
			minConf = v
		}
	}
	return &LLMMatcher{
		apiKey:     key,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		model:      model,
		minConf:    minConf,
	}
}

func (l *LLMMatcher) Model() string {
	if l == nil {
		return ""
	}
	return l.model
}

func (l *LLMMatcher) MinConfidence() float64 {
	if l == nil {
		return 0
	}
	return l.minConf
}

// MatchPairs confirms pre-ranked pairs with the LLM — one call per pair.
// Each call is tiny (~2 titles) so token usage is minimal.
func (l *LLMMatcher) MatchPairs(ctx context.Context, pairs [][2]*models.CanonicalMarket) []*LLMMatchResult {
	var mu sync.Mutex
	var results []*LLMMatchResult
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)

	for _, pair := range pairs {
		wg.Add(1)
		go func(a, b *models.CanonicalMarket) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			judgment, err := l.JudgePair(ctx, a, b)
			if err != nil {
				fmt.Printf("[llm-matcher] WARNING: pair failed %q vs %q: %v\n", a.Title, b.Title, err)
				return
			}
			if judgment == nil || judgment.Decision != LLMDecisionMatch || judgment.Confidence < l.minConf {
				return
			}
			mu.Lock()
			results = append(results, &LLMMatchResult{
				MarketA:    a,
				MarketB:    b,
				Confidence: judgment.Confidence,
				Reasoning:  judgment.Reasoning,
			})
			mu.Unlock()
		}(pair[0], pair[1])
	}
	wg.Wait()
	return results
}

func (l *LLMMatcher) JudgePair(ctx context.Context, source, candidate *models.CanonicalMarket) (*LLMJudgment, error) {
	if source == nil || candidate == nil {
		return nil, fmt.Errorf("source/candidate must be non-nil")
	}
	return l.matchOneAgainstMany(ctx, source, []*models.CanonicalMarket{candidate})
}

// matchOneAgainstMany sends one source market + all candidates in a single LLM call.
func (l *LLMMatcher) matchOneAgainstMany(ctx context.Context, source *models.CanonicalMarket, candidates []*models.CanonicalMarket) (*LLMJudgment, error) {
	// Build candidate list
	var candidateLines string
	for i, c := range candidates {
		candidateLines += fmt.Sprintf("  %d. \"%s\"\n", i+1, c.Title)
	}

	prompt := fmt.Sprintf(`You are a strict prediction market equivalence judge. Decide whether SOURCE and one CANDIDATE resolve the exact same binary question.

SOURCE: "%s"

CANDIDATES:
%s
Return JSON with:
- decision: "match" | "no_match" | "unsure"
- candidate_id: integer (1..N when decision=match, else 0)
- confidence: number 0..1
- reasoning: short sentence

STRICT RULES — read carefully:
- Two markets match ONLY if they resolve the same way for the same outcome. A bet on one should be economically identical to a bet on the other.
- confidence >= 0.9: IDENTICAL question, just worded differently. Example: "Will Arsenal win the Champions League?" = "Champions League Winner — Arsenal" (0.95)
- confidence 0.7-0.89: Very likely the same question with minor ambiguity (e.g. slightly different date ranges).
- confidence < 0.5: NOT a match.

CRITICAL — these are NOT matches (confidence = 0.0):
- Same TOPIC but different SPECIFIC QUESTION: "Will Trump win the 2028 nomination?" ≠ "Trump out as President before 2027?" (different events entirely)
- Same TOPIC but different ENTITY: "Will the Democratic nominee be a woman?" ≠ "2028 Election winner — Gavin Newsom" (different questions)
- Same EVENT but different OUTCOME/SIDE: "Will Team A win?" ≠ "Will Team B win?" (opposing outcomes)
- Same CATEGORY but different THRESHOLD: "Bitcoin above $100k?" ≠ "Bitcoin above $150k?"
- Parent vs sub-question: "Who wins the election?" ≠ "Will candidate X win?" (one is a specific outcome of the other)

Most candidates will NOT match. When in doubt, return decision="no_match" and low confidence.
It is much better to miss a true match than to falsely match two different questions.`,
		source.Title, candidateLines)

	reqBody := map[string]interface{}{
		"model": l.model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.0,
		"max_tokens":  150,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "market_match_decision",
				"strict": true,
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"decision": map[string]any{
							"type": "string",
							"enum": []string{
								string(LLMDecisionMatch),
								string(LLMDecisionNoMatch),
								string(LLMDecisionUnsure),
							},
						},
						"candidate_id": map[string]any{"type": "integer"},
						"confidence":   map[string]any{"type": "number"},
						"reasoning":    map[string]any{"type": "string"},
					},
					"required": []string{"decision", "candidate_id", "confidence", "reasoning"},
					"additionalProperties": false,
				},
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	// Retry up to 4 times on 429, sleeping the duration OpenAI says to wait.
	var resp *http.Response
	var respBody []byte
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+l.apiKey)

		resp, err = l.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("openai request: %w", err)
		}
		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}

		if resp.StatusCode != 429 {
			break
		}

		// Parse Retry-After header (seconds), fall back to exponential backoff.
		wait := time.Duration(1<<uint(attempt)) * time.Second
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			var secs float64
			if _, err := fmt.Sscanf(ra, "%f", &secs); err == nil && secs > 0 {
				wait = time.Duration(secs*1000) * time.Millisecond
			}
		}
		fmt.Printf("[llm-matcher] 429 rate limit, waiting %v before retry %d/3...\n", wait.Round(time.Millisecond), attempt+1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai status %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("parsing openai response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	content := sanitizeJSON(chatResp.Choices[0].Message.Content)

	var llmResult struct {
		Decision    string  `json:"decision"`
		CandidateID int     `json:"candidate_id"`
		Confidence  float64 `json:"confidence"`
		Reasoning   string  `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(content), &llmResult); err != nil {
		return nil, fmt.Errorf("parsing LLM JSON: %w (content=%s)", err, content)
	}
	if llmResult.Confidence < 0 {
		llmResult.Confidence = 0
	}
	if llmResult.Confidence > 1 {
		llmResult.Confidence = 1
	}

	decision := LLMDecision(strings.ToLower(strings.TrimSpace(llmResult.Decision)))
	switch decision {
	case LLMDecisionMatch, LLMDecisionNoMatch, LLMDecisionUnsure:
	default:
		return nil, fmt.Errorf("invalid decision %q", llmResult.Decision)
	}
	if decision != LLMDecisionMatch {
		return &LLMJudgment{
			Decision:   decision,
			Confidence: llmResult.Confidence,
			Reasoning:  llmResult.Reasoning,
		}, nil
	}

	idx := llmResult.CandidateID - 1
	if idx < 0 || idx >= len(candidates) {
		return nil, fmt.Errorf("candidate_id out of range: %d", llmResult.CandidateID)
	}
	return &LLMJudgment{
		Decision:   decision,
		Confidence: llmResult.Confidence,
		Reasoning:  llmResult.Reasoning,
	}, nil
}

func sanitizeJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
