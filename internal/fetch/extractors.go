package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// maxBasicHTTPRunes caps the basic-HTTP text preview, matching the Python
// baseline's 12000-char limit so last-resort output stays bounded.
const maxBasicHTTPRunes = 12000

// maxBasicHTTPBytes caps how much of a page body we read before stripping, a
// defensive bound against pathologically large documents.
const maxBasicHTTPBytes = 4 << 20 // 4 MiB

// tavilyExtract calls Tavily's /extract endpoint and returns the page's
// raw markdown content. Only invoked when a Tavily key is configured.
func (s *Service) tavilyExtract(ctx context.Context, target string) (string, error) {
	body, _ := json.Marshal(map[string]any{"urls": []string{target}, "format": "markdown"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tavilyURL+"/extract", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.tavilyKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("tavily status %d", resp.StatusCode)
	}
	var out struct {
		Results []struct {
			RawContent string `json:"raw_content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Results) > 0 {
		return out.Results[0].RawContent, nil
	}
	return "", nil
}

// firecrawlScrape calls Firecrawl's /scrape endpoint and returns markdown.
// Only invoked when a Firecrawl key is configured.
func (s *Service) firecrawlScrape(ctx context.Context, target string) (string, error) {
	body, _ := json.Marshal(map[string]any{"url": target, "formats": []string{"markdown"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.firecrawlURL+"/scrape", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.firecrawlKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("firecrawl status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Markdown string `json:"markdown"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Data.Markdown, nil
}

// basicHTTPFetch GETs the URL and renders it to bounded plain text. This is
// the always-available last-resort tier; it follows redirects (default
// http.Client behaviour) and reports the final URL in the output header.
func (s *Service) basicHTTPFetch(ctx context.Context, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "openscry-fetch/0.1")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBasicHTTPBytes))
	if err != nil {
		return "", err
	}
	page := string(raw)
	if strings.TrimSpace(page) == "" {
		return "", nil
	}
	title := extractTitle(page)
	if title == "" {
		title = target
	}
	text := stripHTMLToText(page)
	if text == "" {
		return "", nil
	}
	if runes := []rune(text); len(runes) > maxBasicHTTPRunes {
		text = string(runes[:maxBasicHTTPRunes])
	}
	finalURL := target
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return fmt.Sprintf("# %s\n\n来源：%s\n\n%s", title, finalURL, text), nil
}

// HTML-to-text helpers, ported from grok_search/server.py:_strip_html_to_text.
// Go's regexp (RE2) has no backreferences, so <script>/<style> are stripped
// with two separate patterns instead of the Python `</\1>` backreference.
var (
	reScript   = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	reStyle    = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	reBlockEnd = regexp.MustCompile(`(?i)</(p|div|section|article|li|h[1-6]|tr)>`)
	reAnyTag   = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpaces   = regexp.MustCompile(`[ \t]+`)
	reNewlines = regexp.MustCompile(`\n{3,}`)
	reTitle    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// stripHTMLToText reduces an HTML document to bounded plain text: drop
// script/style, turn block-closing tags into newlines, strip remaining tags,
// unescape entities, then collapse runs of spaces and blank lines.
func stripHTMLToText(page string) string {
	out := reScript.ReplaceAllString(page, " ")
	out = reStyle.ReplaceAllString(out, " ")
	out = reBlockEnd.ReplaceAllString(out, "\n")
	out = reAnyTag.ReplaceAllString(out, " ")
	out = html.UnescapeString(out)
	out = reSpaces.ReplaceAllString(out, " ")
	out = reNewlines.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

// extractTitle returns the trimmed, unescaped <title> text, or "" if absent.
func extractTitle(page string) string {
	m := reTitle.FindStringSubmatch(page)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(m[1]))
}
