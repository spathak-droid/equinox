package news

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/equinox/internal/matcher"
)

// Fetcher retrieves news articles from Google News RSS for matched market pairs.
type Fetcher struct {
	client      *http.Client
	maxArticles int

	mu    sync.Mutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	news      *MarketNews
	expiresAt time.Time
}

const cacheTTL = 15 * time.Minute

// NewFetcher creates a news fetcher with the given HTTP timeout and article limit.
func NewFetcher(timeout time.Duration, maxArticles int) *Fetcher {
	if maxArticles <= 0 {
		maxArticles = 5
	}
	return &Fetcher{
		client:      &http.Client{Timeout: timeout},
		maxArticles: maxArticles,
		cache:       make(map[string]*cacheEntry),
	}
}

// FetchForQuery fetches news for a single query string.
func (f *Fetcher) FetchForQuery(ctx context.Context, query string) *MarketNews {
	normalizedQuery := strings.ToLower(strings.TrimSpace(query))
	if cached := f.getCached(normalizedQuery); cached != nil {
		return cached
	}
	mn := f.fetchRSS(ctx, query)
	f.setCache(normalizedQuery, mn)
	return mn
}

// FetchForPairs fetches news for each matched pair, with rate limiting and caching.
func (f *Fetcher) FetchForPairs(ctx context.Context, pairs []*matcher.MatchResult) []*MarketNews {
	results := make([]*MarketNews, len(pairs))

	for i, pair := range pairs {
		query := BuildNewsQuery(pair.MarketA, pair.MarketB)
		normalizedQuery := strings.ToLower(strings.TrimSpace(query))

		// Check cache
		if cached := f.getCached(normalizedQuery); cached != nil {
			results[i] = cached
			continue
		}

		// Rate limit: 500ms between requests
		if i > 0 {
			select {
			case <-ctx.Done():
				results[i] = &MarketNews{Query: query, Error: "context cancelled"}
				continue
			case <-time.After(500 * time.Millisecond):
			}
		}

		mn := f.fetchRSS(ctx, query)
		f.setCache(normalizedQuery, mn)
		results[i] = mn
	}

	return results
}

// fetchRSS fetches and parses a Google News RSS feed for the given query.
func (f *Fetcher) fetchRSS(ctx context.Context, query string) *MarketNews {
	mn := &MarketNews{Query: query}

	rssURL := fmt.Sprintf("https://news.google.com/rss/search?q=%s&hl=en-US&gl=US&ceid=US:en",
		url.QueryEscape(query))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rssURL, nil)
	if err != nil {
		mn.Error = fmt.Sprintf("creating request: %v", err)
		return mn
	}
	req.Header.Set("User-Agent", "Equinox/1.0")

	resp, err := f.client.Do(req)
	if err != nil {
		mn.Error = fmt.Sprintf("fetching RSS: %v", err)
		return mn
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		mn.Error = fmt.Sprintf("RSS returned status %d", resp.StatusCode)
		return mn
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		mn.Error = fmt.Sprintf("reading RSS body: %v", err)
		return mn
	}

	var rss rssResponse
	if err := xml.Unmarshal(body, &rss); err != nil {
		mn.Error = fmt.Sprintf("parsing RSS XML: %v", err)
		return mn
	}

	for _, item := range rss.Channel.Items {
		if len(mn.Articles) >= f.maxArticles {
			break
		}

		article := &NewsArticle{
			Title:   item.Title,
			URL:     item.Link,
			Source:  item.Source,
			Snippet: stripHTML(item.Description),
		}

		// Parse publication date (RSS uses RFC1123 or RFC1123Z)
		if t, err := time.Parse(time.RFC1123, item.PubDate); err == nil {
			article.PublishedAt = t
		} else if t, err := time.Parse(time.RFC1123Z, item.PubDate); err == nil {
			article.PublishedAt = t
		}

		// Extract source from title if not in source element
		// Google News format: "Article Title - Source Name"
		if article.Source == "" {
			if idx := strings.LastIndex(article.Title, " - "); idx > 0 {
				article.Source = article.Title[idx+3:]
				article.Title = article.Title[:idx]
			}
		}

		mn.Articles = append(mn.Articles, article)
	}

	return mn
}

func (f *Fetcher) getCached(key string) *MarketNews {
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry, ok := f.cache[key]; ok && time.Now().Before(entry.expiresAt) {
		return entry.news
	}
	return nil
}

func (f *Fetcher) setCache(key string, mn *MarketNews) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[key] = &cacheEntry{
		news:      mn,
		expiresAt: time.Now().Add(cacheTTL),
	}
}

// htmlTagPattern matches HTML tags for stripping from RSS descriptions.
var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

// stripHTML removes HTML tags from a string, producing a clean text snippet.
func stripHTML(s string) string {
	clean := htmlTagPattern.ReplaceAllString(s, "")
	// Collapse whitespace
	clean = strings.Join(strings.Fields(clean), " ")
	// Truncate long snippets
	if len(clean) > 300 {
		clean = clean[:297] + "..."
	}
	return clean
}
