// Package tracing provides Langfuse-based observability for the Equinox matching pipeline.
//
// All methods are no-ops when LANGFUSE_PUBLIC_KEY is not set, enabling graceful
// degradation in development and CI environments.
package tracing

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/henomis/langfuse-go"
	"github.com/henomis/langfuse-go/model"
)

// Tracer wraps a Langfuse client for pipeline observability.
// When disabled (env vars not set), all methods are safe no-ops.
type Tracer struct {
	client  *langfuse.Langfuse
	enabled bool
}

// New creates a Tracer. Returns a no-op tracer if LANGFUSE_PUBLIC_KEY is not set.
func New(ctx context.Context) *Tracer {
	if os.Getenv("LANGFUSE_PUBLIC_KEY") == "" {
		return &Tracer{enabled: false}
	}

	// langfuse.New reads LANGFUSE_HOST, LANGFUSE_PUBLIC_KEY, LANGFUSE_SECRET_KEY from env.
	client := langfuse.New(ctx)
	return &Tracer{client: client, enabled: true}
}

// Enabled reports whether tracing is active.
func (t *Tracer) Enabled() bool {
	return t != nil && t.enabled
}

// Flush sends any buffered traces to Langfuse. Safe to call when disabled.
func (t *Tracer) Flush(ctx context.Context) {
	if !t.Enabled() {
		return
	}
	t.client.Flush(ctx)
}

// TraceMatchRun starts a trace for a full matching pipeline run.
// The returned RunTrace is used to record spans and scores within this run.
func (t *Tracer) TraceMatchRun(ctx context.Context, query string, marketCount int) (*RunTrace, error) {
	if !t.Enabled() {
		return &RunTrace{tracer: t}, nil
	}

	now := time.Now()
	trace, err := t.client.Trace(&model.Trace{
		Name:      "match-pipeline",
		Timestamp: &now,
		Input:     query,
		Metadata: model.M{
			"market_count": marketCount,
		},
		Tags: []string{"equinox", "matching"},
	})
	if err != nil {
		log.Printf("[tracing] warning: failed to create trace: %v", err)
		return &RunTrace{tracer: t}, nil
	}

	return &RunTrace{
		tracer:  t,
		traceID: trace.ID,
		trace:   trace,
	}, nil
}

// RunTrace represents an active pipeline trace. All methods are no-ops when
// the parent Tracer is disabled or when the trace failed to initialize.
type RunTrace struct {
	tracer  *Tracer
	traceID string
	trace   *model.Trace
}

func (rt *RunTrace) active() bool {
	return rt != nil && rt.tracer.Enabled() && rt.traceID != ""
}

// SpanCompare traces a single market pair comparison within the pipeline run.
func (rt *RunTrace) SpanCompare(a, b string, compositeScore float64, confidence string, explanation string) error {
	if !rt.active() {
		return nil
	}

	now := time.Now()
	span, err := rt.tracer.client.Span(&model.Span{
		TraceID:   rt.traceID,
		Name:      "compare-pair",
		StartTime: &now,
		Input: model.M{
			"market_a": a,
			"market_b": b,
		},
		Output: model.M{
			"composite_score": compositeScore,
			"confidence":      confidence,
			"explanation":     explanation,
		},
		Level: model.ObservationLevelDefault,
	}, nil)
	if err != nil {
		log.Printf("[tracing] warning: failed to create compare span: %v", err)
		return nil
	}

	endTime := time.Now()
	span.EndTime = &endTime
	if _, err := rt.tracer.client.SpanEnd(span); err != nil {
		log.Printf("[tracing] warning: failed to end compare span: %v", err)
	}
	return nil
}

// SpanLLMCall traces an LLM verification call (e.g., GPT-4o pairwise matching).
func (rt *RunTrace) SpanLLMCall(prompt string, response string, llmModel string, durationMs int64, pairsChecked int) error {
	if !rt.active() {
		return nil
	}

	now := time.Now()
	startTime := now.Add(-time.Duration(durationMs) * time.Millisecond)

	gen, err := rt.tracer.client.Generation(&model.Generation{
		TraceID:   rt.traceID,
		Name:      "llm-verify-pairs",
		StartTime: &startTime,
		Model:     llmModel,
		Input:     prompt,
		Metadata: model.M{
			"pairs_checked": pairsChecked,
			"duration_ms":   durationMs,
		},
		Level: model.ObservationLevelDefault,
	}, nil)
	if err != nil {
		log.Printf("[tracing] warning: failed to create LLM generation: %v", err)
		return nil
	}

	endTime := time.Now()
	gen.EndTime = &endTime
	gen.Output = response
	if _, err := rt.tracer.client.GenerationEnd(gen); err != nil {
		log.Printf("[tracing] warning: failed to end LLM generation: %v", err)
	}
	return nil
}

// ScoreRun adds a named score to the overall run trace.
func (rt *RunTrace) ScoreRun(name string, value float64, comment string) error {
	if !rt.active() {
		return nil
	}

	_, err := rt.tracer.client.Score(&model.Score{
		TraceID: rt.traceID,
		Name:    name,
		Value:   value,
		Comment: comment,
	})
	if err != nil {
		log.Printf("[tracing] warning: failed to add score %q: %v", name, err)
	}
	return nil
}

// End finalizes the trace with summary metadata and flushes to Langfuse.
func (rt *RunTrace) End(ctx context.Context, totalPairs int, matchCount int, duration time.Duration) error {
	if !rt.active() {
		return nil
	}

	// Update the trace with output summary
	rt.trace.Output = model.M{
		"total_pairs": totalPairs,
		"match_count": matchCount,
		"duration_ms": duration.Milliseconds(),
	}
	if _, err := rt.tracer.client.Trace(rt.trace); err != nil {
		log.Printf("[tracing] warning: failed to update trace: %v", err)
	}

	// Add summary scores
	_ = rt.ScoreRun("match_count", float64(matchCount),
		fmt.Sprintf("%d matches from %d pairs in %s", matchCount, totalPairs, duration.Round(time.Millisecond)))

	if totalPairs > 0 {
		matchRate := float64(matchCount) / float64(totalPairs)
		_ = rt.ScoreRun("match_rate", matchRate,
			fmt.Sprintf("%.1f%% of pairs matched", matchRate*100))
	}

	rt.tracer.Flush(ctx)
	return nil
}
