package matcher

import (
	"context"
	"fmt"

	"github.com/equinox/internal/models"
)

// LLMEvalCase is one labeled pair used to evaluate pairwise LLM matching quality.
type LLMEvalCase struct {
	Name        string
	TitleA      string
	TitleB      string
	ExpectMatch bool
}

// LLMEvalResult is the model output for one eval pair.
type LLMEvalResult struct {
	Name          string
	ExpectMatch   bool
	PredictedMatch bool
	Decision      LLMDecision
	Confidence    float64
	Reasoning     string
	Err           string
}

// LLMEvalReport summarizes quality metrics over an eval set.
type LLMEvalReport struct {
	Model         string
	MinConfidence float64
	Total         int
	TruePositives int
	FalsePositives int
	TrueNegatives int
	FalseNegatives int
	Accuracy      float64
	Precision     float64
	Recall        float64
	F1            float64
	Results       []LLMEvalResult
}

// DefaultLLMEvalSet provides a compact, high-signal sanity suite for LLM tuning.
func DefaultLLMEvalSet() []LLMEvalCase {
	return []LLMEvalCase{
		{
			Name:        "BTC threshold equivalent",
			TitleA:      "Will Bitcoin hit $100,000 in 2026?",
			TitleB:      "Will BTC reach $100k by end of 2026?",
			ExpectMatch: true,
		},
		{
			Name:        "Trump election equivalent",
			TitleA:      "Will Trump win the 2028 Presidential Election?",
			TitleB:      "Will Donald Trump win the 2028 election?",
			ExpectMatch: true,
		},
		{
			Name:        "Fed rate cut equivalent",
			TitleA:      "Will the Fed cut interest rates in March 2026?",
			TitleB:      "Federal Reserve rate cut in March 2026?",
			ExpectMatch: true,
		},
		{
			Name:        "BTC different thresholds",
			TitleA:      "Will Bitcoin hit $100,000 in 2026?",
			TitleB:      "Will Bitcoin hit $50,000 in 2026?",
			ExpectMatch: false,
		},
		{
			Name:        "Different candidates same election",
			TitleA:      "Will Trump win the 2028 Presidential Election?",
			TitleB:      "Will Harris win the 2028 Presidential Election?",
			ExpectMatch: false,
		},
		{
			Name:        "Same entity different question",
			TitleA:      "Will Trump win the 2028 election?",
			TitleB:      "Will Trump be impeached before 2028?",
			ExpectMatch: false,
		},
		{
			Name:        "Sub-question versus parent market",
			TitleA:      "Who will win the 2028 US presidential election?",
			TitleB:      "Will Trump win the 2028 US presidential election?",
			ExpectMatch: false,
		},
		{
			Name:        "Opposite teams",
			TitleA:      "Will Spain win the 2026 FIFA World Cup?",
			TitleB:      "Will England win the 2026 FIFA World Cup?",
			ExpectMatch: false,
		},
	}
}

// EvaluateLLM runs a labeled evaluation set and returns aggregate metrics.
func EvaluateLLM(ctx context.Context, llm *LLMMatcher, cases []LLMEvalCase) (*LLMEvalReport, error) {
	if llm == nil {
		return nil, fmt.Errorf("LLM matcher unavailable (missing OPENAI_API_KEY?)")
	}
	report := &LLMEvalReport{
		Model:          llm.Model(),
		MinConfidence:  llm.MinConfidence(),
		Total:          len(cases),
		Results:        make([]LLMEvalResult, 0, len(cases)),
	}

	for i, c := range cases {
		a := &models.CanonicalMarket{VenueID: models.VenuePolymarket, VenueMarketID: fmt.Sprintf("eval-poly-%d", i), Title: c.TitleA}
		b := &models.CanonicalMarket{VenueID: models.VenueKalshi, VenueMarketID: fmt.Sprintf("eval-kalshi-%d", i), Title: c.TitleB}

		j, err := llm.JudgePair(ctx, a, b)
		if err != nil {
			report.Results = append(report.Results, LLMEvalResult{
				Name:           c.Name,
				ExpectMatch:    c.ExpectMatch,
				PredictedMatch: false,
				Decision:       LLMDecisionNoMatch,
				Err:            err.Error(),
			})
			if c.ExpectMatch {
				report.FalseNegatives++
			} else {
				report.TrueNegatives++
			}
			continue
		}

		predicted := j != nil && j.Decision == LLMDecisionMatch && j.Confidence >= llm.MinConfidence()
		decision := LLMDecisionNoMatch
		confidence := 0.0
		reasoning := ""
		if j != nil {
			decision = j.Decision
			confidence = j.Confidence
			reasoning = j.Reasoning
		}

		report.Results = append(report.Results, LLMEvalResult{
			Name:           c.Name,
			ExpectMatch:    c.ExpectMatch,
			PredictedMatch: predicted,
			Decision:       decision,
			Confidence:     confidence,
			Reasoning:      reasoning,
		})

		switch {
		case c.ExpectMatch && predicted:
			report.TruePositives++
		case c.ExpectMatch && !predicted:
			report.FalseNegatives++
		case !c.ExpectMatch && predicted:
			report.FalsePositives++
		default:
			report.TrueNegatives++
		}
	}

	tp := float64(report.TruePositives)
	tn := float64(report.TrueNegatives)
	fp := float64(report.FalsePositives)
	fn := float64(report.FalseNegatives)
	total := float64(report.Total)

	if total > 0 {
		report.Accuracy = (tp + tn) / total
	}
	if tp+fp > 0 {
		report.Precision = tp / (tp + fp)
	}
	if tp+fn > 0 {
		report.Recall = tp / (tp + fn)
	}
	if report.Precision+report.Recall > 0 {
		report.F1 = 2 * (report.Precision * report.Recall) / (report.Precision + report.Recall)
	}

	return report, nil
}
