package matcher

import (
	"strings"
	"testing"
)

// TestExtractEventSignature verifies structured field extraction from market titles.
func TestExtractEventSignature(t *testing.T) {
	tests := []struct {
		title     string
		wantEnt   []string // expected entities (subset check)
		wantAct   string
		wantThres string
		wantDir   string
		wantDate  string
		wantMetr  string
		wantComp  string
	}{
		{
			title:     "Will Bitcoin hit $100,000 by June 2026?",
			wantEnt:   []string{"bitcoin"},
			wantAct:   "hit",
			wantThres: "100000",
			wantDir:   "above",
			wantDate:  "jun 2026",
			wantMetr:  "price",
			wantComp:  "reach",
		},
		{
			title:     "Will BTC reach $100k before end of 2026?",
			wantEnt:   []string{"bitcoin"}, // btc → bitcoin via synonyms
			wantAct:   "reach",
			wantThres: "100000",
			wantDir:   "above",
			wantDate:  "2026",
			wantMetr:  "price",
			wantComp:  "reach",
		},
		{
			title:     "Will Trump win the 2028 Presidential Election?",
			wantEnt:   []string{"trump"},
			wantAct:   "win",
			wantThres: "",
			wantDir:   "",
			wantDate:  "2028",
			wantMetr:  "",
			wantComp:  "win",
		},
		{
			title:     "Will the Federal Reserve cut interest rates in March 2026?",
			wantEnt:   []string{"federal reserve"},
			wantAct:   "cut",
			wantThres: "",
			wantDir:   "",
			wantDate:  "mar 2026",
			wantMetr:  "rate",
			wantComp:  "cut",
		},
		{
			title:     "Will Ethereum drop below $2,000 by December 2026?",
			wantEnt:   []string{"ethereum"},
			wantAct:   "drop",
			wantThres: "2000",
			wantDir:   "below",
			wantDate:  "dec 2026",
			wantMetr:  "price",
			wantComp:  "drop_below",
		},
		{
			title:     "Will inflation exceed 5% in 2026?",
			wantEnt:   []string{"inflation"},
			wantAct:   "exceed",
			wantThres: "5.0%",
			wantDir:   "above",
			wantDate:  "2026",
			wantMetr:  "percentage",
			wantComp:  "exceed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			sig := ExtractEventSignature(tt.title)

			// Check entities (subset — we don't require exact match since extraction may find extras)
			for _, want := range tt.wantEnt {
				found := false
				for _, got := range sig.Entities {
					if got == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("entity %q not found in %v", want, sig.Entities)
				}
			}

			if tt.wantAct != "" && sig.Action != tt.wantAct {
				t.Errorf("action: got %q, want %q", sig.Action, tt.wantAct)
			}
			if tt.wantThres != "" && sig.Threshold != tt.wantThres {
				t.Errorf("threshold: got %q, want %q", sig.Threshold, tt.wantThres)
			}
			if tt.wantDir != "" && sig.Direction != tt.wantDir {
				t.Errorf("direction: got %q, want %q", sig.Direction, tt.wantDir)
			}
			if tt.wantDate != "" && sig.DateRef != tt.wantDate {
				t.Errorf("dateRef: got %q, want %q", sig.DateRef, tt.wantDate)
			}
			if tt.wantMetr != "" && sig.Metric != tt.wantMetr {
				t.Errorf("metric: got %q, want %q", sig.Metric, tt.wantMetr)
			}
			if tt.wantComp != "" && sig.Comparator != tt.wantComp {
				t.Errorf("comparator: got %q, want %q", sig.Comparator, tt.wantComp)
			}
		})
	}
}

// TestCanonicalSignature verifies that equivalent titles produce the same hash
// and non-equivalent titles produce different hashes.
func TestCanonicalSignature(t *testing.T) {
	// These pairs should produce the SAME canonical signature
	samePairs := []struct {
		name   string
		titleA string
		titleB string
	}{
		{
			name:   "BTC $100k different phrasing",
			titleA: "Will Bitcoin hit $100,000 by June 2026?",
			titleB: "Will BTC reach $100k before June 2026?",
		},
	}

	for _, tt := range samePairs {
		t.Run("same/"+tt.name, func(t *testing.T) {
			sigA := ExtractEventSignature(tt.titleA)
			sigB := ExtractEventSignature(tt.titleB)
			csA := sigA.CanonicalSignature()
			csB := sigB.CanonicalSignature()
			if csA == "" || csB == "" {
				t.Fatalf("empty signature: A=%q B=%q", csA, csB)
			}
			if csA != csB {
				t.Errorf("signatures should match:\n  A: %s (entities=%v, thresh=%s, comp=%s, date=%s)\n  B: %s (entities=%v, thresh=%s, comp=%s, date=%s)",
					csA, sigA.Entities, sigA.Threshold, sigA.Comparator, sigA.DateRef,
					csB, sigB.Entities, sigB.Threshold, sigB.Comparator, sigB.DateRef)
			}
		})
	}

	// These pairs should produce DIFFERENT canonical signatures
	diffPairs := []struct {
		name   string
		titleA string
		titleB string
	}{
		{
			name:   "BTC different thresholds",
			titleA: "Will Bitcoin hit $100,000 in 2026?",
			titleB: "Will Bitcoin hit $50,000 in 2026?",
		},
		{
			name:   "Different entities same threshold",
			titleA: "Will Bitcoin hit $100,000 in 2026?",
			titleB: "Will Ethereum hit $100,000 in 2026?",
		},
		{
			name:   "Same entity different dates",
			titleA: "Will Bitcoin hit $100,000 by June 2026?",
			titleB: "Will Bitcoin hit $100,000 by December 2026?",
		},
		{
			name:   "Opposite directions",
			titleA: "Will Bitcoin reach $100,000 in 2026?",
			titleB: "Will Bitcoin drop below $100,000 in 2026?",
		},
	}

	for _, tt := range diffPairs {
		t.Run("diff/"+tt.name, func(t *testing.T) {
			sigA := ExtractEventSignature(tt.titleA)
			sigB := ExtractEventSignature(tt.titleB)
			csA := sigA.CanonicalSignature()
			csB := sigB.CanonicalSignature()
			// At least one should have a signature, and they should differ
			if csA != "" && csB != "" && csA == csB {
				t.Errorf("signatures should differ but both are %s\n  A: entities=%v thresh=%s comp=%s date=%s\n  B: entities=%v thresh=%s comp=%s date=%s",
					csA, sigA.Entities, sigA.Threshold, sigA.Comparator, sigA.DateRef,
					sigB.Entities, sigB.Threshold, sigB.Comparator, sigB.DateRef)
			}
		})
	}
}

// TestSignaturesCompatible verifies the hard semantic gate.
func TestSignaturesCompatible(t *testing.T) {
	tests := []struct {
		name   string
		titleA string
		titleB string
		want   bool
	}{
		{
			name:   "same event different phrasing",
			titleA: "Will Bitcoin hit $100,000 in 2026?",
			titleB: "Will BTC reach $100k by 2026?",
			want:   true,
		},
		{
			name:   "different thresholds",
			titleA: "Will Bitcoin hit $100,000 in 2026?",
			titleB: "Will Bitcoin hit $50,000 in 2026?",
			want:   false,
		},
		{
			name:   "different entities",
			titleA: "Will Trump win the 2028 election?",
			titleB: "Will Harris win the 2028 election?",
			want:   false,
		},
		{
			name:   "opposite directions",
			titleA: "Will Bitcoin reach $100,000?",
			titleB: "Will Bitcoin drop below $100,000?",
			want:   false,
		},
		{
			name:   "compatible comparators (reach vs hit)",
			titleA: "Will Bitcoin reach $100,000?",
			titleB: "Will BTC hit $100k?",
			want:   true,
		},
		{
			name:   "no entities — permissive",
			titleA: "Will inflation rise in 2026?",
			titleB: "Will prices rise in 2026?",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigA := ExtractEventSignature(tt.titleA)
			sigB := ExtractEventSignature(tt.titleB)
			got := SignaturesCompatible(sigA, sigB)
			if got != tt.want {
				t.Errorf("SignaturesCompatible = %v, want %v\n  A: entities=%v thresh=%s comp=%s\n  B: entities=%v thresh=%s comp=%s",
					got, tt.want,
					sigA.Entities, sigA.Threshold, sigA.Comparator,
					sigB.Entities, sigB.Threshold, sigB.Comparator)
			}
		})
	}
}

// TestEventMatchScore verifies the scoring function separates same-event from similar-topic.
func TestEventMatchScore(t *testing.T) {
	tests := []struct {
		name   string
		titleA string
		titleB string
		minScr float64 // minimum expected score
		maxScr float64 // maximum expected score
	}{
		{
			name:   "identical event",
			titleA: "Will Bitcoin hit $100,000 in 2026?",
			titleB: "Will BTC reach $100k by 2026?",
			minScr: 0.7,
			maxScr: 1.0,
		},
		{
			name:   "same entity different threshold",
			titleA: "Will Bitcoin hit $100,000 in 2026?",
			titleB: "Will Bitcoin hit $50,000 in 2026?",
			minScr: 0.0,
			maxScr: 0.5, // should be penalized for threshold mismatch
		},
		{
			name:   "completely different",
			titleA: "Will Bitcoin hit $100,000?",
			titleB: "Will Trump win the election?",
			minScr: 0.0,
			maxScr: 0.3,
		},
		{
			name:   "same entity different question",
			titleA: "Will Trump win the 2028 election?",
			titleB: "Will Trump be impeached in 2026?",
			minScr: 0.1,
			maxScr: 0.5, // shared entity but different action/question
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigA := ExtractEventSignature(tt.titleA)
			sigB := ExtractEventSignature(tt.titleB)
			score := EventMatchScore(sigA, sigB)
			if score < tt.minScr || score > tt.maxScr {
				t.Errorf("EventMatchScore = %.3f, want [%.2f, %.2f]\n  A: %+v\n  B: %+v",
					score, tt.minScr, tt.maxScr, sigA, sigB)
			}
		})
	}
}

// TestExtractMetric verifies metric extraction from various market title patterns.
func TestExtractMetric(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"Will Bitcoin price reach $100,000?", "price"},
		{"Will the Fed cut interest rates?", "rate"},
		{"Will Trump's approval rating exceed 50%?", "percentage"},
		{"Will the S&P 500 index close above 5000?", "index"},
		{"How many states will ban TikTok?", "count"},
		{"Will Tesla's market cap reach $2 trillion?", "market_cap"},
	}

	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			got := extractMetric(normalizeForTest(tt.title))
			if got != tt.want {
				t.Errorf("extractMetric(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

// TestExtractRegion verifies region extraction from titles.
func TestExtractRegion(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"Will the United States ban TikTok?", "us"},
		{"Will the Federal Reserve cut rates?", "us"},
		{"Will the Bank of England raise rates?", "uk"},
		{"Will the ECB hold rates steady?", "eu"},
		{"Will China invade Taiwan?", "china"},
		{"Will global GDP growth exceed 3%?", "global"},
		{"Will Bitcoin hit $100k?", ""}, // no region
	}

	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			got := extractRegion(normalizeForTest(tt.title))
			if got != tt.want {
				t.Errorf("extractRegion(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func normalizeForTest(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
