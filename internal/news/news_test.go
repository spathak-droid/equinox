package news

import (
	"encoding/xml"
	"testing"
	"time"

	"github.com/equinox/internal/models"
)

func TestBuildNewsQuery_WithEntities(t *testing.T) {
	a := &models.CanonicalMarket{Title: "Will Bitcoin reach $100,000 by 2026?"}
	b := &models.CanonicalMarket{Title: "Bitcoin to hit $100k before end of 2026"}

	q := BuildNewsQuery(a, b)
	if q == "" {
		t.Fatal("expected non-empty query")
	}
	// Should contain "bitcoin" from entity extraction
	if !containsWord(q, "bitcoin") {
		t.Errorf("expected query to contain 'bitcoin', got %q", q)
	}
	// Should not be too long
	if len(q) > 100 {
		t.Errorf("query too long: %q", q)
	}
}

func TestBuildNewsQuery_KeywordIntersection(t *testing.T) {
	a := &models.CanonicalMarket{Title: "Federal Reserve interest rate decision March"}
	b := &models.CanonicalMarket{Title: "Fed to cut interest rates in March meeting"}

	q := BuildNewsQuery(a, b)
	if q == "" {
		t.Fatal("expected non-empty query")
	}
	t.Logf("query: %q", q)
}

func TestBuildNewsQuery_Fallback(t *testing.T) {
	a := &models.CanonicalMarket{Title: "Something very unique and unusual"}
	b := &models.CanonicalMarket{Title: "Completely different topic here"}

	q := BuildNewsQuery(a, b)
	if q == "" {
		t.Fatal("expected non-empty query from fallback")
	}
	// Should be capped at 6 words
	words := len(splitWords(q))
	if words > 6 {
		t.Errorf("fallback query has %d words, expected <= 6: %q", words, q)
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"<b>Bold</b> text", "Bold text"},
		{"No HTML here", "No HTML here"},
		{"<p>Para</p><br/>Next", "ParaNext"},
		{"", ""},
		{"<a href='url'>Link</a> and <span>span</span>", "Link and span"},
	}
	for _, tt := range tests {
		got := stripHTML(tt.input)
		if got != tt.expected {
			t.Errorf("stripHTML(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestRSSParsing(t *testing.T) {
	xmlData := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <item>
      <title>Bitcoin Surges Past $98K - Reuters</title>
      <link>https://example.com/article1</link>
      <description>&lt;b&gt;Bitcoin&lt;/b&gt; hit a new high</description>
      <pubDate>Mon, 10 Mar 2026 14:30:00 GMT</pubDate>
      <source>Reuters</source>
    </item>
    <item>
      <title>Crypto Markets Rally - Bloomberg</title>
      <link>https://example.com/article2</link>
      <description>Markets see record inflows</description>
      <pubDate>Mon, 10 Mar 2026 12:00:00 GMT</pubDate>
    </item>
  </channel>
</rss>`

	var rss rssResponse
	if err := xml.Unmarshal([]byte(xmlData), &rss); err != nil {
		t.Fatalf("failed to parse RSS: %v", err)
	}

	if len(rss.Channel.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(rss.Channel.Items))
	}

	item := rss.Channel.Items[0]
	if item.Title != "Bitcoin Surges Past $98K - Reuters" {
		t.Errorf("unexpected title: %s", item.Title)
	}
	if item.Source != "Reuters" {
		t.Errorf("unexpected source: %s", item.Source)
	}
	if item.Link != "https://example.com/article1" {
		t.Errorf("unexpected link: %s", item.Link)
	}

	// Test date parsing
	pubDate, err := time.Parse(time.RFC1123, item.PubDate)
	if err != nil {
		t.Errorf("failed to parse pubDate: %v", err)
	}
	if pubDate.Year() != 2026 {
		t.Errorf("expected year 2026, got %d", pubDate.Year())
	}
}

func TestTruncateWords(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"one two three four five six seven", 4, "one two three four"},
		{"short", 10, "short"},
		{"exactly three words", 3, "exactly three words"},
	}
	for _, tt := range tests {
		got := truncateWords(tt.input, tt.n)
		if got != tt.want {
			t.Errorf("truncateWords(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}

func TestSignificantWords(t *testing.T) {
	words := significantWords("Will the Federal Reserve cut rates in 2026?")
	// "will", "the", "in" should be filtered out
	for _, w := range words {
		if w == "will" || w == "the" || w == "in" {
			t.Errorf("stopword %q should have been filtered", w)
		}
	}
	// "federal", "reserve", "cut", "rates", "2026" should remain
	found := map[string]bool{}
	for _, w := range words {
		found[w] = true
	}
	if !found["federal"] || !found["reserve"] {
		t.Errorf("expected 'federal' and 'reserve' in significant words, got %v", words)
	}
}

func TestFetcherCache(t *testing.T) {
	f := NewFetcher(5*time.Second, 5)

	mn := &MarketNews{Query: "test", Articles: []*NewsArticle{{Title: "cached"}}}
	f.setCache("test", mn)

	got := f.getCached("test")
	if got == nil {
		t.Fatal("expected cached result")
	}
	if len(got.Articles) != 1 || got.Articles[0].Title != "cached" {
		t.Errorf("unexpected cached result: %+v", got)
	}

	// Non-existent key
	got = f.getCached("nonexistent")
	if got != nil {
		t.Error("expected nil for non-existent cache key")
	}
}

// helper
func splitWords(s string) []string {
	var out []string
	for _, w := range splitBySpace(s) {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

func splitBySpace(s string) []string {
	return splitOn(s, ' ')
}

func splitOn(s string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			if i > start {
				parts = append(parts, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

func containsWord(s, word string) bool {
	for _, w := range splitWords(s) {
		if w == word {
			return true
		}
	}
	return false
}
