// Package news fetches relevant news articles for matched market pairs via RSS.
//
// Uses Google News RSS as the source. Zero external dependencies — only
// encoding/xml from stdlib. Disabled by default; enable with -news flag or
// NEWS_ENABLED=true.
package news

import (
	"encoding/xml"
	"time"
)

// NewsArticle represents a single news article from an RSS feed.
type NewsArticle struct {
	Title       string    `json:"title"`
	Source      string    `json:"source"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	Snippet     string    `json:"snippet"`
}

// MarketNews holds the news results for a matched market pair.
type MarketNews struct {
	Query    string         `json:"query"`
	Articles []*NewsArticle `json:"articles"`
	Error    string         `json:"error,omitempty"`
}

// RSS XML parsing structs (private — only used by fetcher)

type rssResponse struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	Source      string `xml:"source"`
}
