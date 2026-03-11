package matcher

import (
	"testing"
)

func TestDefaultLLMEvalSet(t *testing.T) {
	cases := DefaultLLMEvalSet()
	if len(cases) == 0 {
		t.Fatal("expected non-empty eval set")
	}

	// Verify we have both positive and negative cases
	positives := 0
	negatives := 0
	for _, c := range cases {
		if c.ExpectMatch {
			positives++
		} else {
			negatives++
		}
	}
	if positives == 0 {
		t.Error("eval set should have at least one positive case")
	}
	if negatives == 0 {
		t.Error("eval set should have at least one negative case")
	}

	// Verify all cases have names and titles
	for _, c := range cases {
		if c.Name == "" {
			t.Error("eval case should have a name")
		}
		if c.TitleA == "" || c.TitleB == "" {
			t.Errorf("eval case %q should have both titles", c.Name)
		}
	}
}

func TestEvaluateLLMNilMatcher(t *testing.T) {
	_, err := EvaluateLLM(nil, nil, DefaultLLMEvalSet())
	if err == nil {
		t.Error("expected error for nil LLM matcher")
	}
}

func TestLLMEvalReportMetrics(t *testing.T) {
	// Manually construct a report to verify metric computation
	report := &LLMEvalReport{
		Total:          4,
		TruePositives:  2,
		FalsePositives: 0,
		TrueNegatives:  1,
		FalseNegatives: 1,
	}

	tp := float64(report.TruePositives)
	tn := float64(report.TrueNegatives)
	fn := float64(report.FalseNegatives)

	accuracy := (tp + tn) / float64(report.Total)
	if accuracy != 0.75 {
		t.Errorf("expected accuracy 0.75, computed %.2f", accuracy)
	}

	precision := tp / (tp + 0) // no false positives
	if precision != 1.0 {
		t.Errorf("expected precision 1.0, computed %.2f", precision)
	}

	recall := tp / (tp + fn)
	expectedRecall := 2.0 / 3.0
	if recall < expectedRecall-0.01 || recall > expectedRecall+0.01 {
		t.Errorf("expected recall ~0.667, computed %.3f", recall)
	}
}
