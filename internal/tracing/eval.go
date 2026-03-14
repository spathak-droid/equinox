package tracing

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/henomis/langfuse-go/model"

	"github.com/equinox/internal/matcher"
	"github.com/equinox/internal/models"
)

// EvalCase defines a golden test case for evaluating the matching pipeline.
type EvalCase struct {
	Name           string
	MarketA        *models.CanonicalMarket
	MarketB        *models.CanonicalMarket
	ExpectedResult string  // "match", "no_match", "probable"
	MinScore       float64 // minimum acceptable composite score (for matches)
	MaxScore       float64 // maximum acceptable composite score (for non-matches)
}

// EvalResult represents the outcome of a single eval case.
type EvalResult struct {
	PairName       string
	Expected       string // "match", "no_match", "probable"
	Got            string
	CompositeScore float64
	Pass           bool
}

// RunEvalSet runs a set of golden pairs through the provided matchFunc and
// traces results to Langfuse. Returns results for programmatic inspection.
//
// matchFunc should call the matcher's compare logic and return a MatchResult.
// This keeps the eval harness decoupled from matcher internals.
func (t *Tracer) RunEvalSet(ctx context.Context, cases []EvalCase, matchFunc func(a, b *models.CanonicalMarket) *matcher.MatchResult) ([]EvalResult, error) {
	if len(cases) == 0 {
		return nil, nil
	}

	start := time.Now()

	// Create a trace for the eval run (no-op if tracing disabled)
	var traceID string
	if t.Enabled() {
		now := time.Now()
		trace, err := t.client.Trace(&model.Trace{
			Name:      "eval-set",
			Timestamp: &now,
			Metadata: model.M{
				"case_count": len(cases),
			},
			Tags: []string{"equinox", "eval"},
		})
		if err != nil {
			log.Printf("[tracing] warning: failed to create eval trace: %v", err)
		} else {
			traceID = trace.ID
		}
	}

	results := make([]EvalResult, 0, len(cases))
	passed := 0

	for _, tc := range cases {
		mr := matchFunc(tc.MarketA, tc.MarketB)

		got := confidenceToEvalString(mr.Confidence)
		pass := evaluateCase(tc, mr)

		result := EvalResult{
			PairName:       tc.Name,
			Expected:       tc.ExpectedResult,
			Got:            got,
			CompositeScore: mr.CompositeScore,
			Pass:           pass,
		}
		results = append(results, result)
		if pass {
			passed++
		}

		// Trace individual eval case as a span
		if t.Enabled() && traceID != "" {
			now := time.Now()
			level := model.ObservationLevelDefault
			if !pass {
				level = model.ObservationLevelWarning
			}
			span, err := t.client.Span(&model.Span{
				TraceID:   traceID,
				Name:      "eval-case",
				StartTime: &now,
				Input: model.M{
					"case_name":  tc.Name,
					"market_a":   tc.MarketA.Title,
					"market_b":   tc.MarketB.Title,
					"expected":   tc.ExpectedResult,
					"min_score":  tc.MinScore,
					"max_score":  tc.MaxScore,
				},
				Output: model.M{
					"got":             got,
					"composite_score": mr.CompositeScore,
					"fuzzy_score":     mr.FuzzyScore,
					"pass":            pass,
					"explanation":     mr.Explanation,
				},
				Level: level,
			}, nil)
			if err != nil {
				log.Printf("[tracing] warning: failed to create eval span: %v", err)
			} else {
				endTime := time.Now()
				span.EndTime = &endTime
				if _, err := t.client.SpanEnd(span); err != nil {
					log.Printf("[tracing] warning: failed to end eval span: %v", err)
				}
			}
		}
	}

	// Record summary scores
	if t.Enabled() && traceID != "" {
		passRate := 0.0
		if len(cases) > 0 {
			passRate = float64(passed) / float64(len(cases))
		}
		_, _ = t.client.Score(&model.Score{
			TraceID: traceID,
			Name:    "eval_pass_rate",
			Value:   passRate,
			Comment: fmt.Sprintf("%d/%d cases passed (%.0f%%)", passed, len(cases), passRate*100),
		})
		_, _ = t.client.Score(&model.Score{
			TraceID: traceID,
			Name:    "eval_pass_count",
			Value:   float64(passed),
			Comment: fmt.Sprintf("%d of %d passed in %s", passed, len(cases), time.Since(start).Round(time.Millisecond)),
		})

		t.Flush(ctx)
	}

	return results, nil
}

// confidenceToEvalString maps a MatchConfidence to the eval string format.
func confidenceToEvalString(c matcher.MatchConfidence) string {
	switch c {
	case matcher.ConfidenceMatch:
		return "match"
	case matcher.ConfidenceProbable:
		return "probable"
	case matcher.ConfidenceNoMatch:
		return "no_match"
	default:
		return "unknown"
	}
}

// evaluateCase checks whether a match result satisfies the eval case criteria.
func evaluateCase(tc EvalCase, mr *matcher.MatchResult) bool {
	got := confidenceToEvalString(mr.Confidence)

	// Primary check: confidence level matches expected
	if got != tc.ExpectedResult {
		return false
	}

	// Secondary check: score bounds (if specified)
	if tc.MinScore > 0 && mr.CompositeScore < tc.MinScore {
		return false
	}
	if tc.MaxScore > 0 && mr.CompositeScore > tc.MaxScore {
		return false
	}

	return true
}
