package router

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
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
)

const maxLLMRespSize = 1 * 1024 * 1024 // 1MB limit for OpenAI API responses

// LLMRouteDecision is a structured routing decision returned by the LLM.
type LLMRouteDecision struct {
	SelectedVenue string
	Confidence    float64
	Reasoning     string
}

// LLMRouterJudge asks a chat model to pick a venue from fully-scored candidates.
// It never replaces deterministic routing when confidence is too low or response is invalid.
type LLMRouterJudge struct {
	apiKey        string
	model         string
	minConfidence float64
	httpClient    *http.Client
}

func NewLLMRouterJudge() *LLMRouterJudge {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil
	}
	model := os.Getenv("OPEN_AI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	minConf := 0.70
	if raw := os.Getenv("ROUTER_LLM_MIN_CONFIDENCE"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 && v <= 1 {
			minConf = v
		}
	}
	return &LLMRouterJudge{
		apiKey:        key,
		model:         model,
		minConfidence: minConf,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (j *LLMRouterJudge) Decide(order *Order, pair *matcher.MatchResult, scores []*VenueScore, cfg *config.Config) (*LLMRouteDecision, error) {
	if j == nil || len(scores) < 2 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	side := string(order.Side)
	if side == "" {
		side = string(SideYes)
	}

	type venueInput struct {
		Venue          string  `json:"venue"`
		Title          string  `json:"title"`
		YesPrice       float64 `json:"yes_price"`
		NoPrice        float64 `json:"no_price"`
		LiquidityUSD   float64 `json:"liquidity_usd"`
		Volume24hUSD   float64 `json:"volume24h_usd"`
		Spread         float64 `json:"spread"`
		PriceScore     float64 `json:"price_score"`
		LiquidityScore float64 `json:"liquidity_score"`
		SpreadScore    float64 `json:"spread_score"`
		TotalScore     float64 `json:"total_score"`
	}
	input := map[string]any{
		"order": map[string]any{
			"event_title": order.EventTitle,
			"side":        side,
			"size_usd":    order.SizeUSD,
		},
		"match": map[string]any{
			"confidence_label": pair.Confidence,
			"confidence_score": pair.CompositeScore,
		},
		"weights": map[string]float64{
			"price":     cfg.PriceWeight,
			"liquidity": cfg.LiquidityWeight,
			"spread":    cfg.SpreadWeight,
		},
	}
	var venues []venueInput
	for _, s := range scores {
		m := s.Market
		venues = append(venues, venueInput{
			Venue:          string(m.VenueID),
			Title:          m.Title,
			YesPrice:       m.YesPrice,
			NoPrice:        1.0 - m.YesPrice,
			LiquidityUSD:   m.Liquidity,
			Volume24hUSD:   m.Volume24h,
			Spread:         m.Spread,
			PriceScore:     s.PriceScore,
			LiquidityScore: s.LiquidityScore,
			SpreadScore:    s.SpreadScore,
			TotalScore:     s.TotalScore,
		})
	}
	input["venues"] = venues

	inJSON, _ := json.Marshal(input)
	prompt := fmt.Sprintf(`You are a strict routing judge for binary prediction markets.
Pick ONE venue from the provided venue list for the order.

Rules:
- For BUY YES, lower yes_price is better. For BUY NO, lower no_price is better.
- Liquidity must be sufficient for order size; insufficient liquidity is a major penalty.
- Tighter spread is better.
- Use provided weights to trade off factors.
- Return only valid JSON.

Input:
%s`, string(inJSON))

	reqBody := map[string]any{
		"model": j.model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.0,
		"max_tokens":  180,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "route_decision",
				"strict": true,
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"selected_venue": map[string]any{"type": "string"},
						"confidence":     map[string]any{"type": "number"},
						"reasoning":      map[string]any{"type": "string"},
					},
					"required":             []string{"selected_venue", "confidence", "reasoning"},
					"additionalProperties": false,
				},
			},
		},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+j.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxLLMRespSize))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	}

	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, err
	}
	if len(chat.Choices) == 0 {
		return nil, fmt.Errorf("empty choices")
	}
	content := strings.TrimSpace(chat.Choices[0].Message.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var out struct {
		SelectedVenue string  `json:"selected_venue"`
		Confidence    float64 `json:"confidence"`
		Reasoning     string  `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, fmt.Errorf("parse LLM routing json: %w", err)
	}
	if out.Confidence < 0 {
		out.Confidence = 0
	}
	if out.Confidence > 1 {
		out.Confidence = 1
	}
	if out.Confidence < j.minConfidence {
		return nil, nil
	}
	validVenue := false
	for _, s := range scores {
		if strings.EqualFold(string(s.Market.VenueID), out.SelectedVenue) {
			validVenue = true
			out.SelectedVenue = string(s.Market.VenueID)
			break
		}
	}
	if !validVenue {
		return nil, nil
	}
	return &LLMRouteDecision{
		SelectedVenue: out.SelectedVenue,
		Confidence:    out.Confidence,
		Reasoning:     out.Reasoning,
	}, nil
}

