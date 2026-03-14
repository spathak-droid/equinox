// Package storage provides persistence backends for Equinox markets.
// This file implements a Qdrant vector database client for semantic search.
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

// QdrantClient is a lightweight REST client for Qdrant vector DB.
type QdrantClient struct {
	baseURL    string
	apiKey     string
	collection string
	httpClient *http.Client
}

// NewQdrantClient creates a new Qdrant REST client.
func NewQdrantClient(baseURL, apiKey, collection string) *QdrantClient {
	return &QdrantClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		collection: collection,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// EnsureCollection creates the collection if it doesn't exist.
// Uses cosine distance and 1536 dimensions (text-embedding-3-small).
func (q *QdrantClient) EnsureCollection(ctx context.Context, vectorSize int) error {
	// Check if collection exists
	exists, err := q.collectionExists(ctx)
	if err != nil {
		return fmt.Errorf("checking collection: %w", err)
	}
	if exists {
		return nil
	}

	body := map[string]any{
		"vectors": map[string]any{
			"size":     vectorSize,
			"distance": "Cosine",
		},
	}
	_, err = q.doRequest(ctx, "PUT", "/collections/"+q.collection, body)
	if err != nil {
		return fmt.Errorf("creating collection %q: %w", q.collection, err)
	}

	// Create payload index on venue_id for filtering
	indexBody := map[string]any{
		"field_name":   "venue_id",
		"field_schema": "keyword",
	}
	_, err = q.doRequest(ctx, "PUT", "/collections/"+q.collection+"/index", indexBody)
	if err != nil {
		fmt.Printf("[qdrant] WARNING: failed to create venue_id index: %v\n", err)
	}

	fmt.Printf("[qdrant] Created collection %q (dim=%d, cosine)\n", q.collection, vectorSize)
	return nil
}

func (q *QdrantClient) collectionExists(ctx context.Context) (bool, error) {
	resp, err := q.doRawRequest(ctx, "GET", "/collections/"+q.collection, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return false, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}
	body, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, body)
}

// QdrantPoint is a single vector point for upsert.
type QdrantPoint struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

// UpsertPoints upserts a batch of points into the collection.
func (q *QdrantClient) UpsertPoints(ctx context.Context, points []QdrantPoint) error {
	if len(points) == 0 {
		return nil
	}

	// Qdrant REST API expects batch upsert
	body := map[string]any{
		"points": points,
	}
	_, err := q.doRequest(ctx, "PUT", "/collections/"+q.collection+"/points", body)
	if err != nil {
		return fmt.Errorf("upserting %d points: %w", len(points), err)
	}
	return nil
}

// QdrantSearchResult is a single search hit.
type QdrantSearchResult struct {
	ID      string         `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

// Search finds the top-K nearest vectors to the query vector.
func (q *QdrantClient) Search(ctx context.Context, vector []float32, limit int, filter map[string]any) ([]QdrantSearchResult, error) {
	body := map[string]any{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
	}
	if filter != nil {
		body["filter"] = filter
	}

	respBody, err := q.doRequest(ctx, "POST", "/collections/"+q.collection+"/points/search", body)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}

	var resp struct {
		Result []QdrantSearchResult `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}
	return resp.Result, nil
}

// CollectionInfo returns basic info about the collection.
type CollectionInfo struct {
	PointsCount int `json:"points_count"`
}

// GetCollectionInfo returns stats about the collection.
func (q *QdrantClient) GetCollectionInfo(ctx context.Context) (*CollectionInfo, error) {
	respBody, err := q.doRequest(ctx, "GET", "/collections/"+q.collection, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			PointsCount int `json:"points_count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, err
	}
	return &CollectionInfo{PointsCount: resp.Result.PointsCount}, nil
}

func (q *QdrantClient) doRequest(ctx context.Context, method, path string, body any) ([]byte, error) {
	resp, err := q.doRawRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qdrant %s %s returned %d: %s", method, path, resp.StatusCode, respBody)
	}
	return respBody, nil
}

func (q *QdrantClient) doRawRequest(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, q.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if q.apiKey != "" {
		req.Header.Set("api-key", q.apiKey)
	}

	return q.httpClient.Do(req)
}
