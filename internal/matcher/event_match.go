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
// The LLM decides which markets are equivalent; fuzzy scores only rerank.
func (m *Matcher) pairChildMarkets(evA, evB *models.CanonicalEvent) []*MatchResult {
	// If either event has only one market, pair directly
	if len(evA.Markets) == 1 && len(evB.Markets) == 1 {
		r := m.compare(evA.Markets[0], evB.Markets[0])
		r.Confidence = ConfidenceMatch
		if r.CompositeScore < 0.5 {
			r.CompositeScore = 0.5
		}
		r.Explanation = "Single market per event — direct pair | " + r.Explanation
		return []*MatchResult{r}
	}

	// Try LLM-based child pairing first
	if m.cfg.OpenAIAPIKey != "" {
		llmPairs := m.pairChildMarketsViaLLM(evA, evB)
		if len(llmPairs) > 0 {
			return llmPairs
		}
		fmt.Printf("[matcher/child] LLM returned no pairs for %q × %q, falling back to fuzzy\n",
			evA.EventTitle, evB.EventTitle)
	}

	// Fallback: fuzzy reranking (no filtering — all cross pairs considered)
	return m.pairChildMarketsFuzzy(evA, evB)
}

// pairChildMarketsViaLLM asks the LLM which child markets across two matched
// events refer to the same specific outcome. Fuzzy scores rerank the result.
func (m *Matcher) pairChildMarketsViaLLM(evA, evB *models.CanonicalEvent) []*MatchResult {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Build prompt
	var sb strings.Builder
	sb.WriteString("You are pairing prediction market contracts within the SAME event across two venues.\n")
	sb.WriteString(fmt.Sprintf("Event: %q\n\n", evA.EventTitle))
	sb.WriteString("Each venue has multiple contracts (outcomes) under this event.\n")
	sb.WriteString("Match contracts that bet on the EXACT SAME outcome.\n\n")
	sb.WriteString("CRITICAL RULES:\n")
	sb.WriteString("- 'Republican' is NOT 'Democratic' — different parties\n")
	sb.WriteString("- 'Trump' is NOT 'Newsom' — different candidates\n")
	sb.WriteString("- '$80,000' is NOT '$100,000' — different thresholds\n")
	sb.WriteString("- Only match contracts where a YES on one side means the same thing as YES on the other\n")
	sb.WriteString("- Each contract can match at most ONE from the other venue\n")
	sb.WriteString("- If no contracts match, return []\n\n")

	marketLabel := func(mkt *models.CanonicalMarket) string {
		if mkt.Subtitle != "" && mkt.Subtitle != mkt.Title {
			return fmt.Sprintf("%s — %s", mkt.Title, mkt.Subtitle)
		}
		return mkt.Title
	}

	sb.WriteString("Venue A contracts:\n")
	for i, mkt := range evA.Markets {
		sb.WriteString(fmt.Sprintf("  %d. %s\n", i, marketLabel(mkt)))
	}
	sb.WriteString("\nVenue B contracts:\n")
	for i, mkt := range evB.Markets {
		sb.WriteString(fmt.Sprintf("  %d. %s\n", i, marketLabel(mkt)))
	}

	sb.WriteString("\nReturn ONLY a JSON array of matched pairs:\n")
	sb.WriteString(`[{"a": <index>, "b": <index>, "reason": "brief why"}]`)
	sb.WriteString("\nReturn [] if no contracts match. Return ONLY the JSON array.\n")

	prompt := sb.String()

	verifyModel := "gpt-4o-mini"
	if m.cfg.OpenAIModel == "gpt-4o" || m.cfg.OpenAIModel == "gpt-4-turbo" {
		verifyModel = m.cfg.OpenAIModel
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
		fmt.Printf("[matcher/child-llm] marshal error: %v\n", err)
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Printf("[matcher/child-llm] request error: %v\n", err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.cfg.OpenAIAPIKey)

	resp, err := eventHTTPClient.Do(req)
	if err != nil {
		fmt.Printf("[matcher/child-llm] API error: %v\n", err)
		return nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		fmt.Printf("[matcher/child-llm] read error: %v\n", err)
		return nil
	}

	if resp.StatusCode != 200 {
		fmt.Printf("[matcher/child-llm] OpenAI %d: %s\n", resp.StatusCode, string(respBody))
		return nil
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil || len(chatResp.Choices) == 0 {
		fmt.Printf("[matcher/child-llm] parse error: %v\n", err)
		return nil
	}

	content := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var llmPairs []llmEventPair
	if err := json.Unmarshal([]byte(content), &llmPairs); err != nil {
		fmt.Printf("[matcher/child-llm] JSON parse error: %v (content: %s)\n", err, content)
		return nil
	}

	fmt.Printf("[matcher/child-llm] %q: LLM paired %d contracts\n", evA.EventTitle, len(llmPairs))

	var results []*MatchResult
	for _, p := range llmPairs {
		if p.A < 0 || p.A >= len(evA.Markets) || p.B < 0 || p.B >= len(evB.Markets) {
			continue
		}
		a := evA.Markets[p.A]
		b := evB.Markets[p.B]

		r := m.compare(a, b)
		r.Confidence = ConfidenceMatch
		if r.CompositeScore < 0.5 {
			r.CompositeScore = 0.5
		}
		r.Explanation = fmt.Sprintf("LLM child-pair: %s | %s", p.Reason, r.Explanation)
		results = append(results, r)
	}

	// Rerank by fuzzy score (best pairs first)
	sort.Slice(results, func(i, j int) bool { return results[i].CompositeScore > results[j].CompositeScore })
	return results
}

// pairChildMarketsFuzzy is the no-LLM fallback. Uses fuzzy scoring to rerank
// all possible cross-pairs, then deduplicates (each market in at most one pair).
func (m *Matcher) pairChildMarketsFuzzy(evA, evB *models.CanonicalEvent) []*MatchResult {
	type scored struct {
		a, b     *models.CanonicalMarket
		subScore float64
	}

	var candidates []scored
	for _, a := range evA.Markets {
		textA := a.Subtitle
		if textA == "" {
			textA = a.Title
		}
		for _, b := range evB.Markets {
			textB := b.Subtitle
			if textB == "" {
				textB = b.Title
			}
			fs := fuzzyTitleScore(textA, textB)
			candidates = append(candidates, scored{a: a, b: b, subScore: fs})
		}
	}

	// Sort by fuzzy score descending
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].subScore > candidates[j].subScore })

	// Deduplicate: each market in at most one pair
	usedA := map[string]bool{}
	usedB := map[string]bool{}
	var pairs []*MatchResult
	for _, c := range candidates {
		if usedA[c.a.VenueMarketID] || usedB[c.b.VenueMarketID] {
			continue
		}
		// Only accept if fuzzy score indicates real similarity
		if c.subScore < 0.4 {
			continue
		}
		usedA[c.a.VenueMarketID] = true
		usedB[c.b.VenueMarketID] = true

		r := m.compare(c.a, c.b)
		r.Confidence = ConfidenceMatch
		if r.CompositeScore < 0.4 {
			r.CompositeScore = 0.4
		}
		r.Explanation = fmt.Sprintf("Fuzzy child-pair (score=%.2f) | %s", c.subScore, r.Explanation)
		pairs = append(pairs, r)
	}

	fmt.Printf("[matcher/child-fuzzy] %q × %q → %d pairs (from %d×%d markets)\n",
		evA.EventTitle, evB.EventTitle, len(pairs), len(evA.Markets), len(evB.Markets))
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
