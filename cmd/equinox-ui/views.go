package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
	"github.com/equinox/internal/news"
	"github.com/equinox/internal/router"
)

// buildPageData takes pre-computed match pairs and builds the PageData for the template.
func buildPageData(cfg *config.Config, ctx context.Context, m *matcher.Matcher, allMarkets []*models.CanonicalMarket, pairs []*matcher.MatchResult, venueCounts map[models.VenueID]int, query string) (*PageData, error) {
	if len(pairs) > maxDisplayPairs {
		pairs = pairs[:maxDisplayPairs]
	}

	var diagnosisMsg string
	if len(pairs) == 0 {
		rejected := m.TopRejectedPairs(allMarkets, 5)
		for i, rj := range rejected {
			fmt.Printf("[equinox-ui] reject #%d score=%.3f fuzzy=%.3f | A=%q | B=%q | reason=%s\n",
				i+1, rj.CompositeScore, rj.FuzzyScore,
				rj.MarketA.Title, rj.MarketB.Title, rj.Explanation)
		}
		diagnosisMsg = buildDiagnosis(venueCounts, rejected)
	}

	r := router.New(cfg)

	// Fetch news for all pairs (non-blocking, best-effort) — only first article per pair
	newsFetcher := news.NewFetcher(cfg.HTTPTimeout, 1)
	pairNews := newsFetcher.FetchForPairs(ctx, pairs)

	var pairViews []PairView
	for i, p := range pairs {
		order := &router.Order{
			EventTitle: p.MarketA.Title,
			Side:       router.SideYes,
			SizeUSD:    cfg.DefaultOrderSize,
		}
		decision := r.Route(order, p)
		pv := PairView{
			MarketA:        toMarketView(p.MarketA),
			MarketB:        toMarketView(p.MarketB),
			Confidence:     string(p.Confidence),
			FuzzyScore:     p.FuzzyScore,
			EmbeddingScore: p.EmbeddingScore,
			CompositeScore: p.CompositeScore,
			Explanation:    p.Explanation,
			SelectedVenue:  string(decision.SelectedVenue.VenueID),
			RoutingReason:  decision.Explanation,
			NewsQuery:      news.BuildNewsQuery(p.MarketA, p.MarketB),
		}
		if i < len(pairNews) && pairNews[i] != nil {
			pv.NewsArticles = toNewsArticleViews(pairNews[i])
		}
		pairViews = append(pairViews, pv)
	}

	matchCount, probableCount := 0, 0
	for _, pv := range pairViews {
		switch pv.Confidence {
		case string(matcher.ConfidenceMatch):
			matchCount++
		case string(matcher.ConfidenceProbable):
			probableCount++
		}
	}

	return &PageData{
		SearchQuery:      query,
		Pairs:            pairViews,
		VenueCounts:      venueCounts,
		MatchCount:       matchCount,
		ProbableCount:    probableCount,
		DiagnosisMessage: diagnosisMsg,
		HasQuery:         true,
	}, nil
}

// matchToPairView converts a single MatchResult to a PairView with routing and news.
func matchToPairView(cfg *config.Config, r *router.Router, p *matcher.MatchResult) PairView {
	order := &router.Order{
		EventTitle: p.MarketA.Title,
		Side:       router.SideYes,
		SizeUSD:    cfg.DefaultOrderSize,
	}
	decision := r.Route(order, p)
	pv := PairView{
		MarketA:        toMarketView(p.MarketA),
		MarketB:        toMarketView(p.MarketB),
		Confidence:     string(p.Confidence),
		FuzzyScore:     p.FuzzyScore,
		EmbeddingScore: p.EmbeddingScore,
		CompositeScore: p.CompositeScore,
		Explanation:    p.Explanation,
		SelectedVenue:  string(decision.SelectedVenue.VenueID),
		RoutingReason:  decision.Explanation,
		NewsQuery:      news.BuildNewsQuery(p.MarketA, p.MarketB),
	}
	return pv
}

func toNewsArticleViews(mn *news.MarketNews) []NewsArticleView {
	if mn == nil {
		return nil
	}
	views := make([]NewsArticleView, 0, len(mn.Articles))
	for _, a := range mn.Articles {
		views = append(views, NewsArticleView{
			Title:  a.Title,
			Source: a.Source,
			URL:    a.URL,
			Age:    formatNewsAge(a.PublishedAt),
		})
	}
	return views
}

func formatNewsAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// buildDiagnosis generates a human-readable explanation of why no matches were found.
func buildDiagnosis(venueCounts map[models.VenueID]int, rejected []*matcher.MatchResult) string {
	polyCount := venueCounts[models.VenuePolymarket]
	kalshiCount := venueCounts[models.VenueKalshi]

	if polyCount == 0 && kalshiCount == 0 {
		return "Neither venue returned markets for this query."
	}
	if polyCount == 0 {
		return fmt.Sprintf("Only Kalshi returned markets (%d). Polymarket had no results for this query, so no cross-venue comparison is possible.", kalshiCount)
	}
	if kalshiCount == 0 {
		return fmt.Sprintf("Only Polymarket returned markets (%d). Kalshi had no results for this query, so no cross-venue comparison is possible.", polyCount)
	}
	if len(rejected) == 0 {
		return fmt.Sprintf("Both venues returned markets (Polymarket: %d, Kalshi: %d) but no cross-venue pairs could be formed.", polyCount, kalshiCount)
	}

	datePenalized, lowSemantic := 0, 0
	var highestComposite float64
	for _, r := range rejected {
		if r.DatePenalty > 0.5 {
			datePenalized++
		}
		if r.CompositeScore < 0.35 {
			lowSemantic++
		}
		if r.CompositeScore > highestComposite {
			highestComposite = r.CompositeScore
		}
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("Both venues have markets (Polymarket: %d, Kalshi: %d) but the questions they ask are different.", polyCount, kalshiCount))
	if datePenalized > 0 && datePenalized == len(rejected) {
		parts = append(parts, "All top candidates have resolution dates far apart, suggesting the venues may be tracking different time periods for similar events.")
	} else if lowSemantic == len(rejected) {
		parts = append(parts, "The market titles have low semantic similarity — the venues appear to be asking fundamentally different questions about this topic.")
	} else {
		parts = append(parts, fmt.Sprintf("The closest pair scored %.2f (threshold: 0.45). The markets may cover different aspects of the same topic.", highestComposite))
	}
	return strings.Join(parts, " ")
}

func toMarketView(m *models.CanonicalMarket) MarketView {
	res := ""
	if m.ResolutionDate != nil {
		res = m.ResolutionDate.Format("2006-01-02")
	}
	createdAt, updatedAt := "", ""
	if !m.CreatedAt.IsZero() {
		createdAt = m.CreatedAt.Format("2006-01-02 15:04:05")
	}
	if !m.UpdatedAt.IsZero() {
		updatedAt = m.UpdatedAt.Format("2006-01-02 15:04:05")
	}

	title := fixKalshiDisplayTitle(m)

	return MarketView{
		Venue:              string(m.VenueID),
		VenueMarketID:      m.VenueMarketID,
		VenueYesTokenID:    m.VenueYesTokenID,
		Title:              title,
		Category:           m.Category,
		Status:             string(m.Status),
		Description:        m.Description,
		Tags:               strings.Join(m.Tags, ", "),
		ImageURL:           marketImageURL(m),
		YesPrice:           m.YesPrice,
		NoPrice:            m.NoPrice,
		Liquidity:          m.Liquidity,
		Spread:             m.Spread,
		ResolutionDate:     res,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
		Volume24h:          m.Volume24h,
		OpenInterest:       m.OpenInterest,
		ResolutionRaw:      m.ResolutionCriteria,
		RawPayloadB64:      base64.StdEncoding.EncodeToString(m.RawPayload),
		VenueLink:          marketVenueLink(m),
		VenueSearchLink:    marketVenueSearchLink(m),
		VenueSearchLinkAlt: marketVenueSearchLinkAlt(m),
	}
}

// fixKalshiDisplayTitle repairs Kalshi titles that have "::" party/affiliation labels
// instead of actual candidate/entity names (e.g. "Election winner? — :: Democratic").
// It extracts the real name from raw_payload's rules_primary field.
func fixKalshiDisplayTitle(m *models.CanonicalMarket) string {
	if m.VenueID != models.VenueKalshi || !strings.Contains(m.Title, " — ::") {
		return m.Title
	}
	// Parse raw payload to get rules_primary
	if len(m.RawPayload) == 0 {
		return m.Title
	}
	var raw struct {
		RulesPrimary string `json:"rules_primary"`
	}
	if err := json.Unmarshal(m.RawPayload, &raw); err != nil {
		return m.Title
	}
	entity := extractEntityFromKalshiRules(raw.RulesPrimary)
	if entity == "" {
		return m.Title
	}
	// Replace the ":: ..." suffix with the extracted entity name
	if idx := strings.Index(m.Title, " — ::"); idx >= 0 {
		return m.Title[:idx] + " — " + entity
	}
	return m.Title
}

// extractEntityFromKalshiRules pulls the entity name from "If <name> is/wins/..." patterns.
func extractEntityFromKalshiRules(rules string) string {
	if !strings.HasPrefix(rules, "If ") {
		return ""
	}
	rest := rules[3:]
	verbs := []string{" is ", " wins ", " receives ", " finishes ", " becomes ", " gets ", " has ", " does "}
	bestIdx := -1
	for _, v := range verbs {
		idx := strings.Index(rest, v)
		if idx > 0 && (bestIdx == -1 || idx < bestIdx) {
			bestIdx = idx
		}
	}
	if bestIdx <= 0 {
		return ""
	}
	return strings.TrimSpace(rest[:bestIdx])
}

// kalshiDefaultIcon is used when Kalshi markets have no image from the API.
const kalshiDefaultIcon = "https://kalshi-fallback-images.s3.amazonaws.com/structured_icons/diamond.webp"

func marketImageURL(m *models.CanonicalMarket) string {
	if m.ImageURL != "" {
		return m.ImageURL
	}
	if m.VenueID == models.VenueKalshi {
		return kalshiDefaultIcon
	}
	return ""
}

func marketVenueLink(m *models.CanonicalMarket) string {
	switch m.VenueID {
	case models.VenuePolymarket:
		slug := m.VenueSlug
		if slug == "" {
			slug = m.VenueMarketID
		}
		if m.VenueEventTicker != "" {
			return "https://polymarket.com/event/" + url.PathEscape(m.VenueEventTicker) + "/" + url.PathEscape(slug)
		}
		return "https://polymarket.com/market/" + url.PathEscape(slug)
	case models.VenueKalshi:
		return marketVenueKalshiLink(m)
	default:
		return ""
	}
}

func marketVenueSearchLink(m *models.CanonicalMarket) string {
	term := url.QueryEscape(m.Title)
	switch m.VenueID {
	case models.VenuePolymarket:
		return "https://polymarket.com/markets?search=" + term
	case models.VenueKalshi:
		return "https://kalshi.com/browse?search=" + term
	default:
		return ""
	}
}

func marketVenueSearchLinkAlt(m *models.CanonicalMarket) string {
	switch m.VenueID {
	case models.VenueKalshi:
		if m.VenueMarketID == "" {
			return ""
		}
		return "https://kalshi.com/browse?search=" + url.QueryEscape(strings.ToLower(m.VenueMarketID))
	default:
		return ""
	}
}

func marketVenueKalshiLink(m *models.CanonicalMarket) string {
	seriesTicker := strings.TrimSpace(strings.ToLower(m.VenueSeriesTicker))
	eventTicker := strings.TrimSpace(strings.ToLower(m.VenueEventTicker))
	eventTitle := strings.TrimSpace(m.VenueEventTitle)

	// Derive series_ticker from event_ticker if missing.
	// Kalshi pattern: event_ticker = series_ticker + "-" + suffix (e.g. KXOSCARPIC-26)
	if seriesTicker == "" && eventTicker != "" {
		if idx := strings.LastIndex(eventTicker, "-"); idx > 0 {
			seriesTicker = eventTicker[:idx]
		}
	}

	// Use event title for slug; fall back to market title stripped of " — subtitle"
	if eventTitle == "" {
		eventTitle = m.Title
		if idx := strings.Index(eventTitle, " — "); idx >= 0 {
			eventTitle = eventTitle[:idx]
		}
	}

	if seriesTicker != "" && eventTicker != "" && eventTitle != "" {
		slug := kalshiTitleSlug(eventTitle)
		return "https://kalshi.com/markets/" + url.PathEscape(seriesTicker) + "/" + slug + "/" + url.PathEscape(eventTicker)
	}
	if m.Title != "" {
		return "https://kalshi.com/browse?search=" + url.QueryEscape(m.Title)
	}
	return ""
}

func kalshiTitleSlug(title string) string {
	s := strings.ToLower(title)
	var buf strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			buf.WriteRune(r)
		} else {
			buf.WriteByte('-')
		}
	}
	result := buf.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	return strings.Trim(result, "-")
}

// isGarbageMarket returns true for markets that should be filtered from display:
// combo/parlay titles, bracket outcomes, dead markets with 0% pricing, etc.
func isGarbageMarket(m *models.CanonicalMarket) bool {
	// Combo/parlay markets with "yes X, no Y" titles
	if strings.HasPrefix(m.Title, "yes ") || strings.HasPrefix(m.Title, "no ") {
		return true
	}
	// Very short/empty titles
	if len(m.Title) < 5 {
		return true
	}
	// Bracket/range outcome markets: "Bitcoin price range on Mar 20? — $63,000 to $63,999"
	// These are individual slices of a multi-outcome market, not standalone questions.
	if isBracketOutcome(m.Title) {
		return true
	}
	return false
}

// isBracketOutcome detects Kalshi-style bracket markets where the title contains
// " — " followed by a numeric range, dollar amount, or specific outcome label.
// Examples:
//   - "Bitcoin price range on Mar 20, 2026? — $63,000 to $63,999.99"
//   - "Will the Philadelphia win the 2026 Pro Basketball Finals? — Philadelphia"
//   - "Fed funds rate after March meeting? — 400 to 424.99"
func isBracketOutcome(title string) bool {
	idx := strings.Index(title, " — ")
	if idx < 0 {
		return false
	}
	suffix := strings.TrimSpace(title[idx+len(" — "):])
	if len(suffix) == 0 {
		return false
	}
	// If the suffix starts with $, a digit, or common range patterns → bracket
	if suffix[0] == '$' {
		return true
	}
	if suffix[0] >= '0' && suffix[0] <= '9' {
		return true
	}
	// Suffix like "Yes", "No", "Under X", "Over X" → bracket outcome
	suffixLower := strings.ToLower(suffix)
	for _, prefix := range []string{"yes", "no", "under ", "over ", "above ", "below ", "more than", "less than", "at least", "fewer than"} {
		if strings.HasPrefix(suffixLower, prefix) {
			return true
		}
	}
	return false
}
