// Package matcher — event_match.go implements LLM-based event-level matching.
//
// Instead of fuzzy/rule-based matching on event titles (which fails on things
// like "Group E" vs "Group F"), we send raw event titles to an LLM and ask it
// which events are the same real-world event.
//
// Flow:
//  1. Group markets into events (models.GroupByEvent)
//  2. Send event titles from both venues to LLM in one batch
//  3. LLM returns which events match
//  4. Within matched events, pair child markets by subtitle similarity
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

// EventMatchResult pairs two matched events and their child market pairings.
type EventMatchResult struct {
	EventA      *models.CanonicalEvent
	EventB      *models.CanonicalEvent
	Confidence  MatchConfidence
	Score       float64
	Explanation string

	// Paired child markets within the matched events
	MarketPairs []*MatchResult
}

// llmEventPair is a single match returned by the LLM.
type llmEventPair struct {
	A      int    `json:"a"`
	B      int    `json:"b"`
	Reason string `json:"reason"`
}

// MatchEvents compares events across venues using LLM.
// If no OpenAI key is configured, falls back to exact title match only.
func (m *Matcher) MatchEvents(eventsA, eventsB []*models.CanonicalEvent) []*EventMatchResult {
	if len(eventsA) == 0 || len(eventsB) == 0 {
		return nil
	}

	var pairs []llmEventPair

	if m.cfg.OpenAIAPIKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var err error
		pairs, err = matchEventsViaLLM(ctx, m.cfg, eventsA, eventsB)
		if err != nil {
			fmt.Printf("[matcher/event] LLM matching failed: %v, falling back to exact match\n", err)
			pairs = matchEventsExact(eventsA, eventsB)
		}
	} else {
		fmt.Println("[matcher/event] No OPENAI_API_KEY set, using exact title match only")
		pairs = matchEventsExact(eventsA, eventsB)
	}

	var results []*EventMatchResult
	for _, p := range pairs {
		if p.A < 0 || p.A >= len(eventsA) || p.B < 0 || p.B >= len(eventsB) {
			continue
		}
		evA := eventsA[p.A]
		evB := eventsB[p.B]

		er := &EventMatchResult{
			EventA:      evA,
			EventB:      evB,
			Confidence:  ConfidenceMatch,
			Score:       1.0,
			Explanation: fmt.Sprintf("LLM match: %s | %q ≈ %q", p.Reason, evA.EventTitle, evB.EventTitle),
		}

		// Pair child markets within matched events
		er.MarketPairs = m.pairChildMarkets(evA, evB)
		results = append(results, er)
	}

	return results
}

// matchEventsViaLLM sends event titles to OpenAI and asks which are the same event.
func matchEventsViaLLM(ctx context.Context, cfg *config.Config, eventsA, eventsB []*models.CanonicalEvent) ([]llmEventPair, error) {
	// Build the prompt
	var sb strings.Builder
	sb.WriteString("You are matching prediction market events across two different venues.\n")
	sb.WriteString("Given events from Venue A and Venue B, identify which events refer to the EXACT SAME real-world event.\n\n")
	sb.WriteString("CRITICAL RULES:\n")
	sb.WriteString("- Only match events that are about the EXACT same thing\n")
	sb.WriteString("- 'Group E' is NOT 'Group F' — different groups are different events\n")
	sb.WriteString("- 'Qualifiers' is NOT 'Winner' — qualifying vs winning are different events\n")
	sb.WriteString("- 'Semifinals' is NOT 'Finals' — different stages are different events\n")
	sb.WriteString("- Ignore minor phrasing differences like '2026 FIFA World Cup Winner' vs 'Men's World Cup winner'\n")
	sb.WriteString("- Each event can match at most ONE event from the other venue\n\n")

	sb.WriteString("Venue A events:\n")
	for i, ev := range eventsA {
		title := ev.EventTitle
		dateStr := ""
		if ev.ResolutionDate != nil {
			dateStr = fmt.Sprintf(" (closes: %s)", ev.ResolutionDate.Format("2006-01-02"))
		}
		sb.WriteString(fmt.Sprintf("  %d. %q%s [%d markets]\n", i, title, dateStr, len(ev.Markets)))
	}

	sb.WriteString("\nVenue B events:\n")
	for i, ev := range eventsB {
		title := ev.EventTitle
		dateStr := ""
		if ev.ResolutionDate != nil {
			dateStr = fmt.Sprintf(" (closes: %s)", ev.ResolutionDate.Format("2006-01-02"))
		}
		sb.WriteString(fmt.Sprintf("  %d. %q%s [%d markets]\n", i, title, dateStr, len(ev.Markets)))
	}

	sb.WriteString("\nReturn ONLY a JSON array of matched pairs. Each pair has:\n")
	sb.WriteString(`- "a": index from Venue A (0-based)`)
	sb.WriteString("\n")
	sb.WriteString(`- "b": index from Venue B (0-based)`)
	sb.WriteString("\n")
	sb.WriteString(`- "reason": brief explanation of why they match`)
	sb.WriteString("\n\n")
	sb.WriteString("If no events match, return an empty array: []\n")
	sb.WriteString("Return ONLY the JSON array, no other text.\n")

	prompt := sb.String()
	fmt.Printf("[matcher/event] Sending %d × %d events to LLM for matching...\n", len(eventsA), len(eventsB))

	// Call OpenAI Chat API
	reqBody := map[string]interface{}{
		"model": cfg.OpenAIModel,
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

	resp, err := http.DefaultClient.Do(req)
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
	// Strip markdown code fences if present
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	fmt.Printf("[matcher/event] LLM response: %s\n", content)

	var pairs []llmEventPair
	if err := json.Unmarshal([]byte(content), &pairs); err != nil {
		return nil, fmt.Errorf("parsing LLM pairs: %w (content: %s)", err, content)
	}

	fmt.Printf("[matcher/event] LLM found %d event matches\n", len(pairs))
	return pairs, nil
}

// matchEventsExact is the fallback when no LLM is available.
// Only matches events with identical titles (case-insensitive).
func matchEventsExact(eventsA, eventsB []*models.CanonicalEvent) []llmEventPair {
	var pairs []llmEventPair
	for i, a := range eventsA {
		titleA := strings.ToLower(strings.TrimSpace(a.EventTitle))
		if titleA == "" {
			continue
		}
		for j, b := range eventsB {
			titleB := strings.ToLower(strings.TrimSpace(b.EventTitle))
			if titleA == titleB {
				pairs = append(pairs, llmEventPair{A: i, B: j, Reason: "exact title match"})
				break // each A matches at most one B
			}
		}
	}
	return pairs
}

// pairChildMarkets pairs markets within two matched events by subtitle similarity.
// Runs full compare() on each pair so all component scores (fuzzy, entity, date, etc.) are populated.
func (m *Matcher) pairChildMarkets(evA, evB *models.CanonicalEvent) []*MatchResult {
	// If either event has only one market, pair directly via compare()
	if len(evA.Markets) == 1 && len(evB.Markets) == 1 {
		r := m.compare(evA.Markets[0], evB.Markets[0])
		// Events already matched by LLM, so override confidence
		r.Confidence = ConfidenceMatch
		if r.CompositeScore < 0.5 {
			r.CompositeScore = 0.5
		}
		r.Explanation = "Single market per event — direct pair | " + r.Explanation
		return []*MatchResult{r}
	}

	// For multi-market events, find best subtitle matches then run compare()
	type scored struct {
		a, b     *models.CanonicalMarket
		subScore float64
	}
	var candidates []scored

	for _, a := range evA.Markets {
		for _, b := range evB.Markets {
			subA := a.Subtitle
			subB := b.Subtitle
			if subA == "" {
				subA = a.Title
			}
			if subB == "" {
				subB = b.Title
			}

			// Exact subtitle match
			if strings.EqualFold(strings.TrimSpace(subA), strings.TrimSpace(subB)) {
				candidates = append(candidates, scored{a: a, b: b, subScore: 1.0})
				continue
			}

			// Fuzzy subtitle match as fallback
			fs := fuzzyTitleScore(subA, subB)
			if fs >= 0.5 {
				candidates = append(candidates, scored{a: a, b: b, subScore: fs})
			}
		}
	}

	// Sort by subtitle score descending
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].subScore > candidates[j-1].subScore; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	// Deduplicate: each market in at most one pair
	usedA := map[string]bool{}
	usedB := map[string]bool{}
	var pairs []*MatchResult
	for _, c := range candidates {
		idA := c.a.VenueMarketID
		idB := c.b.VenueMarketID
		if usedA[idA] || usedB[idB] {
			continue
		}
		usedA[idA] = true
		usedB[idB] = true

		// Run full compare() to populate all component scores
		r := m.compare(c.a, c.b)
		r.Confidence = ConfidenceMatch // events already matched by LLM
		r.Explanation = fmt.Sprintf("Event-paired (%q ≈ %q, sub=%.2f) | %s",
			c.a.Subtitle, c.b.Subtitle, c.subScore, r.Explanation)
		pairs = append(pairs, r)
	}

	return pairs
}

// FlattenEventMatches converts event-level results into a flat list of market pairs
// for compatibility with the existing routing/display pipeline.
func FlattenEventMatches(eventResults []*EventMatchResult) []*MatchResult {
	var all []*MatchResult
	for _, er := range eventResults {
		all = append(all, er.MarketPairs...)
	}

	// Sort by composite score descending
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].CompositeScore > all[j-1].CompositeScore; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}

	return all
}
