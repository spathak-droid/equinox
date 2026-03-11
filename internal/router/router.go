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
	"os"
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
	cfg      *config.Config
	useLLM   bool
	llmJudge *LLMRouterJudge
}

// New returns a Router with the given configuration.
func New(cfg *config.Config) *Router {
	useLLM := strings.EqualFold(os.Getenv("ROUTER_USE_LLM"), "true") || os.Getenv("ROUTER_USE_LLM") == "1"
	j := NewLLMRouterJudge()
	if j == nil {
		useLLM = false
	}
	return &Router{
		cfg:      cfg,
		useLLM:   useLLM,
		llmJudge: j,
	}
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

	var llmChoice *LLMRouteDecision
	if r.useLLM && r.llmJudge != nil {
		if choice, err := r.llmJudge.Decide(order, pair, scores, r.cfg); err == nil && choice != nil {
			llmChoice = choice
			for _, s := range scores {
				if strings.EqualFold(string(s.Market.VenueID), llmChoice.SelectedVenue) {
					decision.SelectedVenue = s.Market
					decision.FinalScore = s.TotalScore
					break
				}
			}
		}
	}

	decision.Explanation = r.buildExplanation(decision, scores)
	if llmChoice != nil {
		decision.Explanation += "\n\n── LLM Judge ───────────────────────────────────────────\n"
		decision.Explanation += fmt.Sprintf("Model: %s | confidence=%.2f | selected=%s\n",
			r.llmJudge.model, llmChoice.Confidence, llmChoice.SelectedVenue)
		decision.Explanation += fmt.Sprintf("Reason: %s\n", llmChoice.Reasoning)
	}
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

// buildExplanation constructs a structured, human-readable routing decision log
// that explains not just the scores but *why* each factor matters for this specific trade.
func (r *Router) buildExplanation(d *RoutingDecision, scores []*VenueScore) string {
	var sb strings.Builder
	order := d.Order
	winner := d.SelectedVenue
	var loser *VenueScore
	for _, s := range scores {
		if s.Market.VenueID != winner.VenueID {
			loser = s
			break
		}
	}
	var winnerScore *VenueScore
	for _, s := range scores {
		if s.Market.VenueID == winner.VenueID {
			winnerScore = s
			break
		}
	}

	sb.WriteString("═══════════════════════════════════════════════════════════\n")
	sb.WriteString("ROUTING DECISION\n")
	sb.WriteString("═══════════════════════════════════════════════════════════\n")

	// Order context
	sideLabel := "YES"
	if order.Side == SideNo {
		sideLabel = "NO"
	}
	sb.WriteString(fmt.Sprintf("Order:   BUY %s on \"%s\" for $%.0f\n", sideLabel, order.EventTitle, order.SizeUSD))
	sb.WriteString(fmt.Sprintf("Markets: %s vs %s\n", d.MatchedPair.MarketA.VenueID, d.MatchedPair.MarketB.VenueID))
	sb.WriteString(fmt.Sprintf("Match:   %s (confidence=%.3f)\n", d.MatchedPair.Confidence, d.MatchedPair.CompositeScore))

	// Per-venue breakdown
	sb.WriteString("\n── Venue Comparison ────────────────────────────────────\n")
	for _, s := range scores {
		m := s.Market
		marker := "  "
		if m.VenueID == winner.VenueID {
			marker = "▶ "
		}

		// Cost per share
		var costPerShare float64
		if order.Side == SideYes {
			costPerShare = m.YesPrice
		} else {
			costPerShare = 1.0 - m.YesPrice
		}
		sharesForOrder := 0.0
		if costPerShare > 0 {
			sharesForOrder = order.SizeUSD / costPerShare
		}

		sb.WriteString(fmt.Sprintf("%s[%s] score=%.4f\n", marker, m.VenueID, s.TotalScore))
		sb.WriteString(fmt.Sprintf("    Price:     %s share costs $%.4f → $%.0f buys ~%.0f shares\n",
			sideLabel, costPerShare, order.SizeUSD, sharesForOrder))
		sb.WriteString(fmt.Sprintf("    Liquidity: $%.0f available", m.Liquidity))
		if m.Liquidity < order.SizeUSD {
			sb.WriteString(fmt.Sprintf(" ⚠️  NOT enough for $%.0f order (%.0f%% filled)",
				order.SizeUSD, (m.Liquidity/order.SizeUSD)*100))
		} else {
			sb.WriteString(fmt.Sprintf(" ✓ covers $%.0f order fully", order.SizeUSD))
		}
		sb.WriteString("\n")
		if m.Spread == 0 {
			sb.WriteString("    Spread:    no data (scored neutral)\n")
		} else {
			bps := m.Spread * 10000
			sb.WriteString(fmt.Sprintf("    Spread:    %.4f (%.0f bps)", m.Spread, bps))
			if m.Spread < 0.005 {
				sb.WriteString(" — very tight\n")
			} else if m.Spread < 0.02 {
				sb.WriteString(" — reasonable\n")
			} else if m.Spread < 0.05 {
				sb.WriteString(" — wide\n")
			} else {
				sb.WriteString(" — very wide, execution cost will be high\n")
			}
		}
	}

	// Weights
	sb.WriteString(fmt.Sprintf("\n── Weights ─────────────────────────────────────────────\n"))
	sb.WriteString(fmt.Sprintf("   Price=%.0f%%  Liquidity=%.0f%%  Spread=%.0f%%\n",
		r.cfg.PriceWeight*100, r.cfg.LiquidityWeight*100, r.cfg.SpreadWeight*100))

	// Plain-English reasoning
	sb.WriteString(fmt.Sprintf("\n── Why %s? ─────────────────────────────────────────\n", winner.VenueID))

	reasons := buildReasons(order, winnerScore, loser)
	for i, reason := range reasons {
		sb.WriteString(fmt.Sprintf("   %d. %s\n", i+1, reason))
	}

	// Estimated execution summary
	var winnerCost float64
	if order.Side == SideYes {
		winnerCost = winner.YesPrice
	} else {
		winnerCost = 1.0 - winner.YesPrice
	}
	winnerShares := 0.0
	if winnerCost > 0 {
		winnerShares = order.SizeUSD / winnerCost
	}
	potentialPayout := winnerShares * 1.0 // each share pays $1 if correct
	potentialProfit := potentialPayout - order.SizeUSD

	sb.WriteString(fmt.Sprintf("\n── Estimated Execution ─────────────────────────────────\n"))
	sb.WriteString(fmt.Sprintf("   Venue:           %s\n", winner.VenueID))
	sb.WriteString(fmt.Sprintf("   Side:            BUY %s\n", sideLabel))
	sb.WriteString(fmt.Sprintf("   Cost per share:  $%.4f\n", winnerCost))
	sb.WriteString(fmt.Sprintf("   Order size:      $%.0f\n", order.SizeUSD))
	sb.WriteString(fmt.Sprintf("   Shares:          ~%.0f\n", winnerShares))
	sb.WriteString(fmt.Sprintf("   If correct:      $%.0f payout ($%.0f profit, %.0f%% return)\n",
		potentialPayout, potentialProfit, (potentialProfit/order.SizeUSD)*100))
	if winner.Liquidity > 0 && winner.Liquidity < order.SizeUSD {
		sb.WriteString(fmt.Sprintf("   ⚠️  Warning: only $%.0f liquidity — you may only fill %.0f%% of this order at the quoted price.\n",
			winner.Liquidity, (winner.Liquidity/order.SizeUSD)*100))
	}

	sb.WriteString("═══════════════════════════════════════════════════════════\n")

	return sb.String()
}

// buildReasons generates plain-English explanations for why the winner beat the loser.
func buildReasons(order *Order, winner, loser *VenueScore) []string {
	if winner == nil || loser == nil {
		return []string{"Only one venue available for this market."}
	}

	var reasons []string

	// Price comparison
	var winCost, loseCost float64
	if order.Side == SideYes {
		winCost = winner.Market.YesPrice
		loseCost = loser.Market.YesPrice
	} else {
		winCost = 1.0 - winner.Market.YesPrice
		loseCost = 1.0 - loser.Market.YesPrice
	}

	if winCost < loseCost {
		saving := loseCost - winCost
		savingPct := (saving / loseCost) * 100
		reasons = append(reasons, fmt.Sprintf("Better price: %s shares cost $%.4f vs $%.4f on %s — %.1f%% cheaper per share.",
			order.Side, winCost, loseCost, loser.Market.VenueID, savingPct))
	} else if winCost > loseCost {
		reasons = append(reasons, fmt.Sprintf("Price is slightly worse ($%.4f vs $%.4f on %s), but other factors compensate.",
			winCost, loseCost, loser.Market.VenueID))
	} else {
		reasons = append(reasons, fmt.Sprintf("Price is identical on both venues ($%.4f per share).", winCost))
	}

	// Liquidity comparison
	winLiq := winner.Market.Liquidity
	loseLiq := loser.Market.Liquidity
	if winLiq > loseLiq*2 && loseLiq < order.SizeUSD {
		reasons = append(reasons, fmt.Sprintf("Much deeper liquidity: $%.0f vs $%.0f on %s — %s cannot fill a $%.0f order without significant slippage.",
			winLiq, loseLiq, loser.Market.VenueID, loser.Market.VenueID, order.SizeUSD))
	} else if winLiq > loseLiq*1.5 {
		reasons = append(reasons, fmt.Sprintf("More liquidity available: $%.0f vs $%.0f on %s, reducing slippage risk for your $%.0f order.",
			winLiq, loseLiq, loser.Market.VenueID, order.SizeUSD))
	} else if loseLiq > winLiq*1.5 {
		reasons = append(reasons, fmt.Sprintf("Liquidity is lower ($%.0f vs $%.0f on %s), but price advantage outweighs this.",
			winLiq, loseLiq, loser.Market.VenueID))
	}

	// Spread comparison
	winSpread := winner.Market.Spread
	loseSpread := loser.Market.Spread
	if winSpread > 0 && loseSpread > 0 {
		if winSpread < loseSpread*0.5 {
			reasons = append(reasons, fmt.Sprintf("Tighter spread: %.0f bps vs %.0f bps on %s — lower hidden execution cost.",
				winSpread*10000, loseSpread*10000, loser.Market.VenueID))
		} else if loseSpread < winSpread*0.5 {
			reasons = append(reasons, fmt.Sprintf("Spread is wider (%.0f bps vs %.0f bps on %s), but this is a minor factor at %.0f%% weight.",
				winSpread*10000, loseSpread*10000, loser.Market.VenueID, 10.0))
		}
	}

	if len(reasons) == 0 {
		reasons = append(reasons, fmt.Sprintf("Scores are close — %s edges out with a marginally better combination of price, liquidity, and spread.",
			winner.Market.VenueID))
	}

	return reasons
}
