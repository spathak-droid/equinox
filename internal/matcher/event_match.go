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
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
)

// reThreshold matches dollar amounts like $150,000 or $71,500.00 or plain numbers like 150000.
var reThreshold = regexp.MustCompile(`\$?([\d,]+(?:\.\d+)?)`)


// eventHTTPClient is used for event-matching LLM requests with a bounded timeout.
var eventHTTPClient = &http.Client{Timeout: 60 * time.Second}

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
func (m *Matcher) MatchEvents(eventsA, eventsB []*models.CanonicalEvent, searchQuery ...string) []*EventMatchResult {
	if len(eventsA) == 0 || len(eventsB) == 0 {
		return nil
	}

	query := ""
	if len(searchQuery) > 0 {
		query = searchQuery[0]
	}

	var pairs []llmEventPair

	if m.cfg.OpenAIAPIKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var err error
		pairs, err = matchEventsViaLLM(ctx, m.cfg, eventsA, eventsB, query)
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
func matchEventsViaLLM(ctx context.Context, cfg *config.Config, eventsA, eventsB []*models.CanonicalEvent, searchQuery ...string) ([]llmEventPair, error) {
	query := ""
	if len(searchQuery) > 0 {
		query = searchQuery[0]
	}

	// Build the prompt
	var sb strings.Builder
	sb.WriteString("You are matching prediction market events across two different venues.\n")
	sb.WriteString("Given events from Venue A and Venue B, identify which events refer to the EXACT SAME real-world event.\n\n")
	if query != "" {
		sb.WriteString(fmt.Sprintf("USER'S SEARCH QUERY: %q\n", query))
		sb.WriteString("IMPORTANT: Only match events that are RELEVANT to the user's search query. Ignore events about unrelated topics.\n\n")
	}
	sb.WriteString("CRITICAL RULES:\n")
	sb.WriteString("- Only match events that are about the EXACT same thing\n")
	sb.WriteString("- 'Group E' is NOT 'Group F' — different groups are different events\n")
	sb.WriteString("- 'Qualifiers' is NOT 'Winner' — qualifying vs winning are different events\n")
	sb.WriteString("- 'Semifinals' is NOT 'Finals' — different stages are different events\n")
	sb.WriteString("- Ignore minor phrasing differences like '2026 FIFA World Cup Winner' vs 'Men's World Cup winner'\n")
	sb.WriteString("- Each event can match at most ONE event from the other venue\n")
	if query != "" {
		sb.WriteString(fmt.Sprintf("- REJECT events not related to %q\n", query))
	}
	sb.WriteString("\n")

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

	// Use gpt-4o for event matching — accuracy matters more than speed/cost.
	// gpt-4.1-nano and other cheap models produce too many false positives
	// (e.g. matching "nominee be a woman" with "election occur").
	verifyModel := "gpt-4o"
	if cfg.OpenAIModel == "gpt-4o" || cfg.OpenAIModel == "gpt-4-turbo" {
		verifyModel = cfg.OpenAIModel
	}

	// Call OpenAI Chat API
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

	resp, err := eventHTTPClient.Do(req)
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

// extractNumericThreshold pulls the largest dollar/number value from a title or subtitle.
// e.g. "Will Bitcoin reach $150,000 in March?" → 150000
//      "$71,500 or above" → 71500
// Returns 0 if no number found.
func extractNumericThreshold(s string) float64 {
	matches := reThreshold.FindAllStringSubmatch(s, -1)
	var best float64
	for _, m := range matches {
		raw := strings.ReplaceAll(m[1], ",", "")
		v, err := strconv.ParseFloat(raw, 64)
		if err == nil && v > best {
			best = v
		}
	}
	return best
}

// thresholdsClose returns true if two thresholds are within 1% of each other.
func thresholdsClose(a, b float64) bool {
	if a == 0 || b == 0 {
		return false
	}
	ratio := a / b
	return ratio >= 0.99 && ratio <= 1.01
}

// pairChildMarkets pairs markets within two matched events.
// Uses three strategies in order:
//  1. Exact threshold match (e.g. both mention $80,000)
//  2. Fuzzy subtitle/title match (lowered to 0.3 since events are LLM-verified)
//  3. For single-market events, direct pair
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

	// Build threshold index for each market
	type marketThreshold struct {
		market    *models.CanonicalMarket
		threshold float64
		text      string // subtitle or title used for fuzzy
	}

	extractMT := func(mkt *models.CanonicalMarket) marketThreshold {
		text := mkt.Subtitle
		if text == "" {
			text = mkt.Title
		}
		// Try subtitle first, then title for threshold
		thr := extractNumericThreshold(text)
		if thr == 0 {
			thr = extractNumericThreshold(mkt.Title)
		}
		return marketThreshold{market: mkt, threshold: thr, text: text}
	}

	mtsA := make([]marketThreshold, len(evA.Markets))
	mtsB := make([]marketThreshold, len(evB.Markets))
	for i, a := range evA.Markets {
		mtsA[i] = extractMT(a)
	}
	for i, b := range evB.Markets {
		mtsB[i] = extractMT(b)
	}

	type scored struct {
		a, b     *models.CanonicalMarket
		subScore float64
		method   string
	}
	var candidates []scored

	for _, a := range mtsA {
		for _, b := range mtsB {
			// Strategy 1: exact threshold match (highest priority)
			if a.threshold > 0 && thresholdsClose(a.threshold, b.threshold) {
				candidates = append(candidates, scored{
					a: a.market, b: b.market,
					subScore: 0.95 + 0.05*(1.0-math.Abs(a.threshold-b.threshold)/math.Max(a.threshold, 1)),
					method:   fmt.Sprintf("threshold=$%.0f≈$%.0f", a.threshold, b.threshold),
				})
				continue
			}

			// Strategy 2: exact subtitle match
			if strings.EqualFold(strings.TrimSpace(a.text), strings.TrimSpace(b.text)) {
				candidates = append(candidates, scored{a: a.market, b: b.market, subScore: 1.0, method: "exact-subtitle"})
				continue
			}

			// Strategy 3: fuzzy match with lowered threshold (0.3 instead of 0.5)
			// Events are already LLM-verified, so we can be more permissive
			fs := fuzzyTitleScore(a.text, b.text)
			if fs >= 0.3 {
				candidates = append(candidates, scored{a: a.market, b: b.market, subScore: fs, method: "fuzzy"})
			}
		}
	}

	// Sort by score descending
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].subScore > candidates[j].subScore })

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
		if r.CompositeScore < 0.4 {
			r.CompositeScore = 0.4
		}
		r.Explanation = fmt.Sprintf("Event-paired [%s] (%q ≈ %q, sub=%.2f) | %s",
			c.method, c.a.Subtitle, c.b.Subtitle, c.subScore, r.Explanation)
		pairs = append(pairs, r)
	}

	fmt.Printf("[matcher/child] %q × %q → %d candidates, %d pairs (from %d×%d markets)\n",
		evA.EventTitle, evB.EventTitle, len(candidates), len(pairs), len(evA.Markets), len(evB.Markets))

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
	sort.Slice(all, func(i, j int) bool { return all[i].CompositeScore > all[j].CompositeScore })

	return all
}
