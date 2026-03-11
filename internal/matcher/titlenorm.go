// Package matcher — titlenorm.go uses OpenAI to normalize market titles into
// a canonical form before Jaccard comparison.
//
// Instead of comparing "Will Spain win the 2026 FIFA World Cup?" against
// "2026 Men's World Cup winner — Spain" directly (low Jaccard due to different
// phrasing), we normalize both to "spain wins 2026 fifa world cup" first.
//
// All titles are batched into a single OpenAI call (cheap, fast, ~500 tokens
// for 50 markets). Results are cached in memory for the process lifetime.
package matcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/equinox/internal/models"
)

// TitleNormalizer normalizes prediction market titles into canonical forms.
type TitleNormalizer struct {
	apiKey     string
	model      string
	httpClient *http.Client

	mu    sync.Mutex
	cache map[string]string // title → normalized
}

type titleBatchItem struct {
	market *models.CanonicalMarket
	title  string
}

// NewTitleNormalizer returns a normalizer backed by OpenAI.
// Returns nil if OPENAI_API_KEY is not set.
func NewTitleNormalizer() *TitleNormalizer {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil
	}
	model := os.Getenv("OPEN_AI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	return &TitleNormalizer{
		apiKey:     key,
		model:      model,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		cache:      make(map[string]string),
	}
}

// EnrichMarkets sets NormalizedTitle on each market that doesn't already have one.
// All uncached titles are sent in a single batched OpenAI call.
// On any error, original titles are left unchanged (graceful degradation).
func (n *TitleNormalizer) EnrichMarkets(ctx context.Context, markets []*models.CanonicalMarket) {
	if n == nil || len(markets) == 0 {
		return
	}

	// Collect titles not yet cached
	var work []titleBatchItem

	n.mu.Lock()
	for _, m := range markets {
		if m.NormalizedTitle != "" {
			continue
		}
		if cached, ok := n.cache[m.Title]; ok {
			m.NormalizedTitle = cached
			continue
		}
		work = append(work, titleBatchItem{m, m.Title})
	}
	n.mu.Unlock()

	if len(work) == 0 {
		return
	}

	// Batch in chunks of 50 to stay well within token limits
	const chunkSize = 50
	for i := 0; i < len(work); i += chunkSize {
		end := i + chunkSize
		if end > len(work) {
			end = len(work)
		}
		chunk := work[i:end]

		normalized, err := n.callBatch(ctx, chunk)
		if err != nil {
			fmt.Printf("[titlenorm] WARNING: batch failed: %v — using original titles\n", err)
			continue
		}

		n.mu.Lock()
		for j, norm := range normalized {
			if j >= len(chunk) {
				break
			}
			n.cache[chunk[j].title] = norm
			chunk[j].market.NormalizedTitle = norm
		}
		n.mu.Unlock()
	}
}

// callBatch sends one OpenAI request normalizing all titles in the chunk.
func (n *TitleNormalizer) callBatch(ctx context.Context, items []titleBatchItem) ([]string, error) {
	// Build numbered list
	var sb strings.Builder
	for i, item := range items {
		fmt.Fprintf(&sb, "%d. %q\n", i+1, item.title)
	}

	prompt := fmt.Sprintf(`Normalize these prediction market titles into short canonical phrases for semantic comparison.

Rules:
- 4-7 words, all lowercase, no punctuation
- Keep: entity names, years, numbers/thresholds, key verbs
- Drop: "Will", "?", articles (the/a/an), "winner", "—", filler words
- Both "Will Spain win the 2026 FIFA World Cup?" and "2026 Men's World Cup winner — Spain" → "spain wins 2026 fifa world cup"
- "Will Bitcoin reach $100k before 2026?" → "bitcoin reaches 100k before 2026"
- "Trump wins 2028 US presidential election" → "trump wins 2028 us presidential election"

Titles:
%s
Return ONLY a JSON array of %d strings in the same order. No explanation.`, sb.String(), len(items))

	reqBody := map[string]interface{}{
		"model": n.model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.0,
		"max_tokens":  len(items) * 20, // ~15 tokens per normalized title
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.apiKey)

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

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
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	content := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	// Strip markdown code fences if present
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var result []string
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("parsing JSON array: %w (content=%s)", err, content)
	}

	if len(result) != len(items) {
		return nil, fmt.Errorf("expected %d normalized titles, got %d", len(items), len(result))
	}

	fmt.Printf("[titlenorm] Normalized %d titles (model=%s)\n", len(items), n.model)
	return result, nil
}
