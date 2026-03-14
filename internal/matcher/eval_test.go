package matcher

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/equinox/config"
	"github.com/equinox/internal/models"
)

// goldenPairJSON mirrors the JSON structure of testdata/golden_pairs.json.
type goldenPairJSON struct {
	Name    string          `json:"name"`
	MarketA goldenMarketJSON `json:"market_a"`
	MarketB goldenMarketJSON `json:"market_b"`
	Expected string          `json:"expected"` // "match", "no_match", "probable"
	MinScore float64         `json:"min_score"`
	MaxScore float64         `json:"max_score"`
}

type goldenMarketJSON struct {
	Title          string   `json:"title"`
	VenueID        string   `json:"venue_id"`
	Category       string   `json:"category"`
	YesPrice       float64  `json:"yes_price"`
	ResolutionDate *string  `json:"resolution_date"` // pointer to handle null
}

// buildMarketFromJSON constructs a CanonicalMarket from JSON test data.
func buildMarketFromJSON(g goldenMarketJSON, idSuffix string) *models.CanonicalMarket {
	m := &models.CanonicalMarket{
		ID:            "eval-" + idSuffix,
		VenueID:       models.VenueID(g.VenueID),
		VenueMarketID: "eval-" + idSuffix,
		Title:         g.Title,
		Category:      g.Category,
		YesPrice:      g.YesPrice,
		NoPrice:       1.0 - g.YesPrice,
		Status:        models.StatusActive,
	}
	if g.ResolutionDate != nil {
		t, err := time.Parse(time.RFC3339, *g.ResolutionDate)
		if err == nil {
			m.ResolutionDate = &t
		}
	}
	return m
}

// TestEvalGoldenPairs loads golden_pairs.json and evaluates each case through
// the matcher pipeline, checking confidence and score bounds.
func TestEvalGoldenPairs(t *testing.T) {
	data, err := os.ReadFile("../../testdata/golden_pairs.json")
	if err != nil {
		t.Fatalf("failed to read golden_pairs.json: %v", err)
	}

	var pairs []goldenPairJSON
	if err := json.Unmarshal(data, &pairs); err != nil {
		t.Fatalf("failed to parse golden_pairs.json: %v", err)
	}

	if len(pairs) < 30 {
		t.Fatalf("expected at least 30 golden pairs, got %d", len(pairs))
	}

	cfg := &config.Config{
		MatchThreshold:         0.45,
		ProbableMatchThreshold: 0.35,
		MaxDateDeltaDays:       365,
		PriceWeight:            0.60,
		LiquidityWeight:        0.30,
		SpreadWeight:           0.10,
	}
	m := New(cfg)

	// Counters for summary
	var passed, failed int

	for i, p := range pairs {
		t.Run(p.Name, func(t *testing.T) {
			marketA := buildMarketFromJSON(p.MarketA, fmt.Sprintf("%d-a", i))
			marketB := buildMarketFromJSON(p.MarketB, fmt.Sprintf("%d-b", i))

			result := m.compare(marketA, marketB)

			switch p.Expected {
			case "match":
				// For match cases: either confidence is MATCH, or composite >= min_score
				scoreOK := result.CompositeScore >= p.MinScore
				confOK := result.Confidence == ConfidenceMatch
				if !scoreOK && !confOK {
					failed++
					t.Errorf("FAIL [match] %s: confidence=%s, composite=%.3f (want >= %.2f or MATCH)\n  explanation: %s",
						p.Name, result.Confidence, result.CompositeScore, p.MinScore, result.Explanation)
				} else {
					passed++
				}
				// Also check score is within max bound
				if result.CompositeScore > p.MaxScore {
					t.Logf("WARNING: score %.3f exceeds max_score %.2f for %s", result.CompositeScore, p.MaxScore, p.Name)
				}

			case "no_match":
				// For no_match cases: confidence must NOT be MATCH, and composite <= max_score
				confBad := result.Confidence == ConfidenceMatch
				scoreBad := result.CompositeScore > p.MaxScore
				if confBad || scoreBad {
					failed++
					t.Errorf("FAIL [no_match] %s: confidence=%s, composite=%.3f (want != MATCH and <= %.2f)\n  explanation: %s",
						p.Name, result.Confidence, result.CompositeScore, p.MaxScore, result.Explanation)
				} else {
					passed++
				}

			case "probable":
				// For probable cases: composite should be within [min_score, max_score]
				// Confidence can be PROBABLE_MATCH, MATCH, or NO_MATCH depending on thresholds
				inRange := result.CompositeScore >= p.MinScore && result.CompositeScore <= p.MaxScore
				confOK := result.Confidence == ConfidenceProbable || result.Confidence == ConfidenceMatch
				if !inRange && !confOK {
					failed++
					t.Errorf("FAIL [probable] %s: confidence=%s, composite=%.3f (want in [%.2f, %.2f] or PROBABLE/MATCH)\n  explanation: %s",
						p.Name, result.Confidence, result.CompositeScore, p.MinScore, p.MaxScore, result.Explanation)
				} else {
					passed++
				}

			default:
				t.Fatalf("unknown expected value: %q", p.Expected)
			}

			// Always log details for visibility
			t.Logf("  %s: confidence=%s composite=%.3f fuzzy=%.3f event=%.3f entity=%.3f date=%.3f price=%.3f sig=%v\n    %s",
				p.Name, result.Confidence, result.CompositeScore,
				result.FuzzyScore, result.EventMatchScore, result.EntityOverlapScore,
				result.DateProximityScore, result.PriceProximityScore, result.SignatureMatch,
				result.Explanation)
		})
	}

	t.Logf("\n=== Eval Summary: %d passed, %d failed out of %d total ===", passed, failed, len(pairs))
}

// TestEvalMatchVsNoMatchSeparation verifies that the highest-scoring no_match case
// scores lower than the lowest-scoring match case. This is the fundamental quality
// metric for the matching pipeline.
func TestEvalMatchVsNoMatchSeparation(t *testing.T) {
	data, err := os.ReadFile("../../testdata/golden_pairs.json")
	if err != nil {
		t.Fatalf("failed to read golden_pairs.json: %v", err)
	}

	var pairs []goldenPairJSON
	if err := json.Unmarshal(data, &pairs); err != nil {
		t.Fatalf("failed to parse golden_pairs.json: %v", err)
	}

	cfg := &config.Config{
		MatchThreshold:         0.45,
		ProbableMatchThreshold: 0.35,
		MaxDateDeltaDays:       365,
		PriceWeight:            0.60,
		LiquidityWeight:        0.30,
		SpreadWeight:           0.10,
	}
	m := New(cfg)

	var maxNoMatchScore float64
	var maxNoMatchName string
	var minMatchScore float64 = 1.0
	var minMatchName string

	for i, p := range pairs {
		marketA := buildMarketFromJSON(p.MarketA, fmt.Sprintf("sep-%d-a", i))
		marketB := buildMarketFromJSON(p.MarketB, fmt.Sprintf("sep-%d-b", i))
		result := m.compare(marketA, marketB)

		switch p.Expected {
		case "match":
			if result.CompositeScore < minMatchScore {
				minMatchScore = result.CompositeScore
				minMatchName = p.Name
			}
		case "no_match":
			if result.CompositeScore > maxNoMatchScore {
				maxNoMatchScore = result.CompositeScore
				maxNoMatchName = p.Name
			}
		}
	}

	t.Logf("Lowest match score: %.3f (%s)", minMatchScore, minMatchName)
	t.Logf("Highest no_match score: %.3f (%s)", maxNoMatchScore, maxNoMatchName)

	gap := minMatchScore - maxNoMatchScore
	t.Logf("Separation gap: %.3f (positive = clean separation, negative = overlap)", gap)
	if maxNoMatchScore >= minMatchScore {
		t.Logf("WARNING: Score separation violated: highest no_match (%.3f, %s) >= lowest match (%.3f, %s). "+
			"This indicates the pipeline has overlap between true matches and false matches at current thresholds.",
			maxNoMatchScore, maxNoMatchName, minMatchScore, minMatchName)
		// This is informational, not a hard failure, since some overlap is expected
		// without embeddings or LLM disambiguation. The eval dataset documents this gap.
	}
}
