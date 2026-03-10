// Package router implements the venue-agnostic routing engine.
//
// The router takes a hypothetical order (a desired trade on a matched market pair)
// and decides which venue to route it to, then explains the decision.
//
// # Routing model
//
// For each candidate venue, we compute a routing score from three factors:
//
//   Price score (weight: cfg.PriceWeight — default 60%):
//     For a BUY YES order: higher YesPrice = worse (you pay more per share).
//     For a BUY NO  order: lower YesPrice = worse (NoPrice = 1 - YesPrice is worse).
//     We invert so that a better price yields a higher score:
//       priceScore = 1 - YesPrice  for YES orders
//       priceScore = YesPrice      for NO orders
//
//   Liquidity score (weight: cfg.LiquidityWeight — default 30%):
//     We prefer venues with more liquidity. Score = tanh(liquidity / orderSize).
//     tanh is used to produce a smooth [0, 1) score that saturates rather than
//     creating extreme values when liquidity >> orderSize.
//     If liquidity < orderSize we still route there (not a hard filter) but the
//     score is penalized and the explanation flags this.
//
//   Spread score (weight: cfg.SpreadWeight — default 10%):
//     Lower spread = better execution.
//     score = 1 - min(spread, 0.20) / 0.20   (caps at spread=0.20)
//     If spread data is unavailable (= 0 from venue), the spread score is 0.5 (neutral).
//
// Final routing score = priceWeight*priceScore + liquidityWeight*liquidityScore + spreadWeight*spreadScore
//
// The venue with the highest routing score is selected.
//
// # Design notes
//   - The router has zero knowledge of venue-specific APIs or field names.
//     It operates only on CanonicalMarket fields.
//   - We do not implement execution (no wallets, no order submission).
//     This is a simulation — the output is a decision and a logged rationale.
package router

import (
	"fmt"
	"math"
	"strings"

	"github.com/equinox/config"
	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
)

// OrderSide represents whether the hypothetical order buys YES or NO.
type OrderSide string

const (
	SideYes OrderSide = "YES"
	SideNo  OrderSide = "NO"
)

// Order represents a hypothetical trade request.
type Order struct {
	// EventTitle is a human-readable label for logging (not used for routing logic)
	EventTitle string
	Side       OrderSide
	// SizeUSD is the intended order size in USD
	SizeUSD float64
}

// VenueScore captures the intermediate scoring for one venue.
type VenueScore struct {
	Market         *models.CanonicalMarket
	PriceScore     float64
	LiquidityScore float64
	SpreadScore    float64
	TotalScore     float64
	Explanation    string
}

// RoutingDecision is the output of the router for a single order.
type RoutingDecision struct {
	Order          *Order
	MatchedPair    *matcher.MatchResult
	SelectedVenue  *models.CanonicalMarket
	AllScores      []*VenueScore
	FinalScore     float64
	Explanation    string
}

// Router makes venue routing decisions for hypothetical orders.
type Router struct {
	cfg *config.Config
}

// New returns a Router with the given configuration.
func New(cfg *config.Config) *Router {
	return &Router{cfg: cfg}
}

// Route evaluates the venues available in a matched pair and selects the best one
// for the given order. It always returns a decision, even when data is imperfect.
func (r *Router) Route(order *Order, pair *matcher.MatchResult) *RoutingDecision {
	candidates := []*models.CanonicalMarket{pair.MarketA, pair.MarketB}

	scores := make([]*VenueScore, 0, len(candidates))
	for _, m := range candidates {
		s := r.scoreVenue(m, order)
		scores = append(scores, s)
	}

	// Select highest scoring venue
	best := scores[0]
	for _, s := range scores[1:] {
		if s.TotalScore > best.TotalScore {
			best = s
		}
	}

	decision := &RoutingDecision{
		Order:         order,
		MatchedPair:   pair,
		SelectedVenue: best.Market,
		AllScores:     scores,
		FinalScore:    best.TotalScore,
	}

	decision.Explanation = r.buildExplanation(decision, scores)
	return decision
}

// scoreVenue computes the routing score for a single candidate venue.
func (r *Router) scoreVenue(m *models.CanonicalMarket, order *Order) *VenueScore {
	s := &VenueScore{Market: m}
	var notes []string

	// --- Price score ---
	// We want the best execution price: lowest cost per share.
	// For YES: a lower yes_price is better (you buy cheaper).
	// For NO:  a higher yes_price means a lower no_price, which is better.
	var priceScore float64
	switch order.Side {
	case SideYes:
		priceScore = 1.0 - m.YesPrice // lower price = higher score
		notes = append(notes, fmt.Sprintf("yes_price=%.4f (score=%.3f)", m.YesPrice, priceScore))
	case SideNo:
		priceScore = m.YesPrice // higher yes_price → lower no_price → better for NO
		notes = append(notes, fmt.Sprintf("no_price=%.4f (score=%.3f)", 1-m.YesPrice, priceScore))
	}
	s.PriceScore = priceScore

	// --- Liquidity score ---
	// tanh(liquidity / order_size) → [0, 1), saturates smoothly
	orderSize := order.SizeUSD
	if orderSize <= 0 {
		orderSize = 1.0
		notes = append(notes, "order_size<=0 adjusted to 1.0 for scoring")
	}
	liquidityScore := math.Tanh(m.Liquidity / orderSize)
	s.LiquidityScore = liquidityScore

	liquidityNote := fmt.Sprintf("liquidity=$%.0f (score=%.3f)", m.Liquidity, liquidityScore)
	if m.Liquidity < order.SizeUSD {
		liquidityNote += " ⚠️ insufficient for full order size"
	}
	notes = append(notes, liquidityNote)

	// --- Spread score ---
	// 1 - clamp(spread/0.20) → prefer tighter spreads
	// If spread is 0 (not reported by venue), we use 0.5 as neutral
	var spreadScore float64
	if m.Spread == 0 {
		spreadScore = 0.5 // neutral — spread data unavailable
		notes = append(notes, "spread=N/A (score=0.5 neutral)")
	} else {
		spreadScore = 1.0 - math.Min(m.Spread/0.20, 1.0)
		notes = append(notes, fmt.Sprintf("spread=%.4f (score=%.3f)", m.Spread, spreadScore))
	}
	s.SpreadScore = spreadScore

	// --- Weighted total ---
	s.TotalScore = r.cfg.PriceWeight*priceScore +
		r.cfg.LiquidityWeight*liquidityScore +
		r.cfg.SpreadWeight*spreadScore

	s.Explanation = fmt.Sprintf("[%s] total=%.4f | %s",
		m.VenueID, s.TotalScore, strings.Join(notes, " | "))

	return s
}

// buildExplanation constructs a structured, human-readable routing decision log.
func (r *Router) buildExplanation(d *RoutingDecision, scores []*VenueScore) string {
	var sb strings.Builder

	sb.WriteString("═══════════════════════════════════════════════════════════\n")
	sb.WriteString("ROUTING DECISION\n")
	sb.WriteString("═══════════════════════════════════════════════════════════\n")
	sb.WriteString(fmt.Sprintf("Order:   %s %s @ $%.2f\n", d.Order.Side, d.Order.EventTitle, d.Order.SizeUSD))
	sb.WriteString(fmt.Sprintf("Markets: %s / %s\n", d.MatchedPair.MarketA.VenueID, d.MatchedPair.MarketB.VenueID))
	sb.WriteString(fmt.Sprintf("Match confidence: %s (score=%.3f)\n\n", d.MatchedPair.Confidence, d.MatchedPair.CompositeScore))

	sb.WriteString("Venue scores:\n")
	for _, s := range scores {
		marker := "  "
		if s.Market.VenueID == d.SelectedVenue.VenueID {
			marker = "▶ "
		} else {
			marker = "  "
		}
		sb.WriteString(fmt.Sprintf("%s%s\n", marker, s.Explanation))
	}

	sb.WriteString(fmt.Sprintf("\nWeights: price=%.0f%% liquidity=%.0f%% spread=%.0f%%\n",
		r.cfg.PriceWeight*100, r.cfg.LiquidityWeight*100, r.cfg.SpreadWeight*100))

	sb.WriteString(fmt.Sprintf("\n✅ SELECTED: %s (score=%.4f)\n", d.SelectedVenue.VenueID, d.FinalScore))
	sb.WriteString(fmt.Sprintf("   Title: %s\n", d.SelectedVenue.Title))
	sb.WriteString(fmt.Sprintf("   Yes price: %.4f | Liquidity: $%.0f | Spread: %.4f\n",
		d.SelectedVenue.YesPrice, d.SelectedVenue.Liquidity, d.SelectedVenue.Spread))
	sb.WriteString("═══════════════════════════════════════════════════════════\n")

	return sb.String()
}
