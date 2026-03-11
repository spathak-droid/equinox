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

// LLMMatcher uses OpenAI to match markets across venues.
type LLMMatcher struct {
	apiKey     string
	httpClient *http.Client
	model      string
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
	return &LLMMatcher{
		apiKey:     key,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		model:      model,
	}
}

// MatchAll takes Polymarket markets as source and compares each against all
// Kalshi candidates in a single LLM call per source market.
func (l *LLMMatcher) MatchAll(ctx context.Context, sourceMarkets, candidateMarkets []*models.CanonicalMarket) []*LLMMatchResult {
	if len(sourceMarkets) == 0 || len(candidateMarkets) == 0 {
		return nil
	}

	fmt.Printf("[llm-matcher] Matching %d source markets against %d candidates using %s (%d API calls)...\n",
		len(sourceMarkets), len(candidateMarkets), l.model, len(sourceMarkets))

	var mu sync.Mutex
	var allResults []*LLMMatchResult
	var wg sync.WaitGroup

	// Limit concurrency to avoid blasting the TPM limit.
	sem := make(chan struct{}, 3)

	for _, src := range sourceMarkets {
		wg.Add(1)
		go func(source *models.CanonicalMarket) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			results, err := l.matchOneAgainstMany(ctx, source, candidateMarkets)
			if err != nil {
				fmt.Printf("[llm-matcher] WARNING: failed for %q: %v\n", source.Title, err)
				return
			}

			mu.Lock()
			allResults = append(allResults, results...)
			mu.Unlock()
		}(src)
	}

	wg.Wait()

	// Sort by confidence descending
	for i := 1; i < len(allResults); i++ {
		for j := i; j > 0 && allResults[j].Confidence > allResults[j-1].Confidence; j-- {
			allResults[j], allResults[j-1] = allResults[j-1], allResults[j]
		}
	}

	fmt.Printf("[llm-matcher] Done. %d results scored.\n", len(allResults))
	return allResults
}

// matchOneAgainstMany sends one source market + all candidates in a single LLM call.
func (l *LLMMatcher) matchOneAgainstMany(ctx context.Context, source *models.CanonicalMarket, candidates []*models.CanonicalMarket) ([]*LLMMatchResult, error) {
	// Build candidate list
	var candidateLines string
	for i, c := range candidates {
		candidateLines += fmt.Sprintf("  %d. \"%s\"\n", i+1, c.Title)
	}

	prompt := fmt.Sprintf(`You are a strict prediction market equivalence judge. Given a SOURCE market and a list of CANDIDATES, determine if any candidate is asking the EXACT SAME question as the source.

SOURCE: "%s"

CANDIDATES:
%s
Respond with ONLY valid JSON (no markdown):
{"id": <candidate number or 0 if none match>, "confidence": <0.0-1.0>, "reasoning": "<one sentence>"}

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

Most candidates will NOT match. When in doubt, return {"id": 0, "confidence": 0.0, "reasoning": "no match"}.
It is much better to miss a true match than to falsely match two different questions.`,
		source.Title, candidateLines)

	reqBody := map[string]interface{}{
		"model": l.model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.0,
		"max_tokens":  150,
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

	content := chatResp.Choices[0].Message.Content

	var llmResult struct {
		ID         int     `json:"id"`
		Confidence float64 `json:"confidence"`
		Reasoning  string  `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(content), &llmResult); err != nil {
		return nil, fmt.Errorf("parsing LLM JSON: %w (content=%s)", err, content)
	}

	// No match found — require at least 0.7 from LLM to consider
	if llmResult.ID <= 0 || llmResult.Confidence < 0.7 {
		return nil, nil
	}

	idx := llmResult.ID - 1
	if idx < 0 || idx >= len(candidates) {
		return nil, nil
	}

	return []*LLMMatchResult{{
		MarketA:    source,
		MarketB:    candidates[idx],
		Confidence: llmResult.Confidence,
		Reasoning:  llmResult.Reasoning,
	}}, nil
}
