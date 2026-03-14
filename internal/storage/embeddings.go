package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// EmbeddingClient calls the OpenAI embeddings API.
type EmbeddingClient struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewEmbeddingClient creates an OpenAI embedding client.
func NewEmbeddingClient(apiKey, model string) *EmbeddingClient {
	return &EmbeddingClient{
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// EmbedBatch embeds multiple texts in a single API call.
// Returns one []float32 per input text.
func (e *EmbeddingClient) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	body := map[string]any{
		"input": texts,
		"model": e.model,
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/embeddings", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding API call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("reading embedding response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embedding API returned %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing embedding response: %w", err)
	}

	// Re-order by index (API may return out of order)
	vecs := make([][]float32, len(texts))
	for _, d := range result.Data {
		if d.Index < len(vecs) {
			vecs[d.Index] = d.Embedding
		}
	}
	return vecs, nil
}

// Embed embeds a single text.
func (e *EmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 || vecs[0] == nil {
		return nil, fmt.Errorf("no embedding returned")
	}
	return vecs[0], nil
}

// VectorDimension returns the expected dimension for the configured model.
func (e *EmbeddingClient) VectorDimension() int {
	switch e.model {
	case "text-embedding-3-large":
		return 3072
	case "text-embedding-3-small":
		return 1536
	case "text-embedding-ada-002":
		return 1536
	default:
		return 1536
	}
}
