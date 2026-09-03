// Package sources separates a Grok answer from its cited sources and
// normalizes heterogeneous upstream citation formats into one consistent
// list. grok2api emits citations in several shapes depending on version and
// model: a trailing "## Sources" heading block, a <details> block, a function
// call like sources([...]), a tail block of bare link lines, or inline
// [[N]](url) markers scattered through the body.
//
// Split tries each strategy in priority order and returns the answer with the
// trailing sources block removed (inline citations are kept in place, since
// they are part of the body's meaning) plus the extracted sources. This is a
// faithful Go port of GrokSearch's sources.py, kept transport-agnostic so the
// CLI and the MCP adapter share one extraction path.
package sources

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Source is a single cited reference. URL is always present; Title and
// Description are best-effort and may be empty.
type Source struct {
	Title       string `json:"title,omitempty"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

var (
	urlPattern      = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `，。、；：！？》）】)]+`)
	mdLinkPattern   = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)]+)\)`)
	headingPattern  = regexp.MustCompile(`(?im)^(?:#{1,6}\s*)?(?:\*\*|__)?\s*(?:sources?|references?|citations?|信源|参考资料|参考|引用|来源列表|来源)\s*[:：]?\s*(?:\*\*|__)?(?:\s*[（(][^)\n]*[)）])?\s*[:：]?\s*$`)
	functionPattern = regexp.MustCompile(`(?im)(^|\n)\s*(sources|source|citations|citation|references|reference|citation_card|source_cards|source_card)\s*\(`)
	inlineCitation  = regexp.MustCompile(`\[\[(\d+)\]\]\((https?://[^)\s]+)\)`)
	listPrefix      = regexp.MustCompile(`^\s*(?:[-*]|\d+\.)\s*`)
	trailingPunct   = ".,;:!?"
)

// Split separates the answer text from its cited sources. It returns the
// (possibly trimmed) answer and the extracted sources. When no sources are
// found it returns the original text and a nil slice.
func Split(text string) (string, []Source) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return "", nil
	}
	for _, strategy := range []func(string) (string, []Source, bool){
		splitFunctionCall,
		splitHeading,
		splitDetailsBlock,
		splitTailLinkBlock,
		splitInlineCitations,
	} {
		if answer, src, ok := strategy(raw); ok {
			return answer, src
		}
	}
	return raw, nil
}

// Merge concatenates multiple source lists, deduplicating by URL while
// preserving first-seen order. 重复 URL 的后到项若带有 Title / Description 而先到项
// 缺失，则回填到先到项：内联引用只有 URL，标题往往来自后面的注解或尾部列表。
func Merge(lists ...[]Source) []Source {
	seen := make(map[string]int)
	var merged []Source
	for _, list := range lists {
		for _, item := range list {
			url := strings.TrimSpace(item.URL)
			if url == "" {
				continue
			}
			if idx, dup := seen[url]; dup {
				if merged[idx].Title == "" && strings.TrimSpace(item.Title) != "" {
					merged[idx].Title = strings.TrimSpace(item.Title)
				}
				if merged[idx].Description == "" && strings.TrimSpace(item.Description) != "" {
					merged[idx].Description = strings.TrimSpace(item.Description)
				}
				continue
			}
			seen[url] = len(merged)
			item.URL = url
			merged = append(merged, item)
		}
	}
	return merged
}

// splitFunctionCall handles a trailing sources(...) / citations(...) call.
func splitFunctionCall(text string) (string, []Source, bool) {
	matches := functionPattern.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return "", nil, false
	}
	// Walk matches from the end: the sources call is expected to be the tail.
	for i := len(matches) - 1; i >= 0; i-- {
		openParen := matches[i][1] - 1 // functionPattern ends with "("
		closeParen, args, ok := balancedCallAtEnd(text, openParen)
		if !ok {
			continue
		}
		_ = closeParen
		src := parseSourcesPayload(args)
		if len(src) == 0 {
			continue
		}
		// The function-name token starts after the leading (^|\n); trim any
		// trailing newline captured by the group.
		answer := strings.TrimRight(text[:matches[i][0]], "\n")
		answer = strings.TrimRight(answer, " \t")
		return answer, src, true
	}
	return "", nil, false
}

// balancedCallAtEnd scans from an opening paren to its matching close paren,
// honoring quoted strings. It only succeeds when the close paren is the last
// non-space content in text (i.e. the call sits at the very end).
func balancedCallAtEnd(text string, openParen int) (int, string, bool) {
	if openParen < 0 || openParen >= len(text) || text[openParen] != '(' {
		return 0, "", false
	}
	depth := 1
	var inString byte // 0 when not in a string, else the quote char
	escape := false
	for idx := openParen + 1; idx < len(text); idx++ {
		ch := text[idx]
		if inString != 0 {
			if escape {
				escape = false
				continue
			}
			if ch == '\\' {
				escape = true
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inString = ch
			continue
		}
		if ch == '(' {
			depth++
			continue
		}
		if ch == ')' {
			depth--
			if depth == 0 {
				if strings.TrimSpace(text[idx+1:]) != "" {
					return 0, "", false
				}
				return idx, text[openParen+1 : idx], true
			}
		}
	}
	return 0, "", false
}

// splitHeading handles a trailing "## Sources" / "参考资料" heading block.
func splitHeading(text string) (string, []Source, bool) {
	matches := headingPattern.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return "", nil, false
	}
	for i := len(matches) - 1; i >= 0; i-- {
		start := matches[i][0]
		src := extractSourcesFromText(text[start:])
		if len(src) == 0 {
			continue
		}
		answer := strings.TrimRight(text[:start], " \t\r\n")
		return answer, src, true
	}
	return "", nil, false
}

// splitDetailsBlock handles a trailing <details>...</details> block.
func splitDetailsBlock(text string) (string, []Source, bool) {
	lower := strings.ToLower(text)
	closeIdx := strings.LastIndex(lower, "</details>")
	if closeIdx == -1 {
		return "", nil, false
	}
	if strings.TrimSpace(text[closeIdx+len("</details>"):]) != "" {
		return "", nil, false
	}
	openIdx := strings.LastIndex(lower[:closeIdx], "<details")
	if openIdx == -1 {
		return "", nil, false
	}
	block := text[openIdx : closeIdx+len("</details>")]
	src := extractSourcesFromText(block)
	if len(src) < 2 {
		return "", nil, false
	}
	answer := strings.TrimRight(text[:openIdx], " \t\r\n")
	return answer, src, true
}

// splitTailLinkBlock handles a trailing run of >=2 link-only lines.
func splitTailLinkBlock(text string) (string, []Source, bool) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return "", nil, false
	}
	idx := len(lines) - 1
	for idx >= 0 && strings.TrimSpace(lines[idx]) == "" {
		idx--
	}
	if idx < 0 {
		return "", nil, false
	}
	tailEnd := idx
	linkCount := 0
	for idx >= 0 {
		line := strings.TrimSpace(lines[idx])
		if line == "" {
			idx--
			continue
		}
		if !isLinkOnlyLine(line) {
			break
		}
		linkCount++
		idx--
	}
	tailStart := idx + 1
	if linkCount < 2 {
		return "", nil, false
	}
	block := strings.Join(lines[tailStart:tailEnd+1], "\n")
	src := extractSourcesFromText(block)
	if len(src) == 0 {
		return "", nil, false
	}
	answer := strings.TrimRight(strings.Join(lines[:tailStart], "\n"), " \t\r\n")
	return answer, src, true
}

// splitInlineCitations harvests grok2api [[N]](url) inline markers. The full
// answer is preserved because inline citations are part of the body's meaning.
func splitInlineCitations(text string) (string, []Source, bool) {
	matches := inlineCitation.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return "", nil, false
	}
	seen := make(map[string]struct{})
	var src []Source
	for _, m := range matches {
		url := strings.TrimRight(strings.TrimSpace(m[2]), trailingPunct)
		if url == "" {
			continue
		}
		if _, dup := seen[url]; dup {
			continue
		}
		seen[url] = struct{}{}
		src = append(src, Source{URL: url})
	}
	if len(src) == 0 {
		return "", nil, false
	}
	return text, src, true
}

func isLinkOnlyLine(line string) bool {
	stripped := strings.TrimSpace(listPrefix.ReplaceAllString(line, ""))
	if stripped == "" {
		return false
	}
	if strings.HasPrefix(stripped, "http://") || strings.HasPrefix(stripped, "https://") {
		return true
	}
	return mdLinkPattern.MatchString(stripped)
}

// parseSourcesPayload parses the args of a sources(...) call. It tries JSON
// first (the common grok2api shape) and falls back to scraping URLs/markdown
// links out of the raw text. Python tuple/single-quote literals that are not
// valid JSON degrade to text extraction.
func parseSourcesPayload(payload string) []Source {
	payload = strings.TrimRight(strings.TrimSpace(payload), ";")
	if payload == "" {
		return nil
	}
	var data any
	if err := json.Unmarshal([]byte(payload), &data); err == nil {
		if src := normalizeSources(data); len(src) > 0 {
			return src
		}
	}
	return extractSourcesFromText(payload)
}

// normalizeSources coerces a decoded JSON value into a source list. It accepts
// a list of strings, [title, url] pairs, or objects with url/href/link keys,
// and an object wrapper carrying a sources/citations/references/urls field.
func normalizeSources(data any) []Source {
	switch v := data.(type) {
	case map[string]any:
		for _, key := range []string{"sources", "citations", "references", "urls"} {
			if inner, ok := v[key]; ok {
				return normalizeSources(inner)
			}
		}
		return normalizeSourceItems([]any{v})
	case []any:
		return normalizeSourceItems(v)
	default:
		return normalizeSourceItems([]any{v})
	}
}

func normalizeSourceItems(items []any) []Source {
	seen := make(map[string]struct{})
	var out []Source
	add := func(s Source) {
		url := strings.TrimSpace(s.URL)
		if url == "" || !(strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
			return
		}
		if _, dup := seen[url]; dup {
			return
		}
		seen[url] = struct{}{}
		s.URL = url
		out = append(out, s)
	}
	for _, item := range items {
		switch it := item.(type) {
		case string:
			for _, u := range extractUniqueURLs(it) {
				add(Source{URL: u})
			}
		case []any:
			if len(it) >= 2 {
				title, _ := it[0].(string)
				url, _ := it[1].(string)
				add(Source{Title: strings.TrimSpace(title), URL: url})
			}
		case map[string]any:
			add(sourceFromObject(it))
		}
	}
	return out
}

func sourceFromObject(obj map[string]any) Source {
	pick := func(keys ...string) string {
		for _, k := range keys {
			if val, ok := obj[k].(string); ok && strings.TrimSpace(val) != "" {
				return strings.TrimSpace(val)
			}
		}
		return ""
	}
	return Source{
		URL:         pick("url", "href", "link"),
		Title:       pick("title", "name", "label"),
		Description: pick("description", "snippet", "content"),
	}
}

// extractSourcesFromText scrapes markdown links first (preserving titles),
// then any remaining bare URLs, deduplicating by URL.
func extractSourcesFromText(text string) []Source {
	seen := make(map[string]struct{})
	var src []Source
	for _, m := range mdLinkPattern.FindAllStringSubmatch(text, -1) {
		url := strings.TrimSpace(m[2])
		if url == "" {
			continue
		}
		if _, dup := seen[url]; dup {
			continue
		}
		seen[url] = struct{}{}
		title := strings.TrimSpace(m[1])
		if title != "" {
			src = append(src, Source{Title: title, URL: url})
		} else {
			src = append(src, Source{URL: url})
		}
	}
	for _, url := range extractUniqueURLs(text) {
		if _, dup := seen[url]; dup {
			continue
		}
		seen[url] = struct{}{}
		src = append(src, Source{URL: url})
	}
	return src
}

func extractUniqueURLs(text string) []string {
	seen := make(map[string]struct{})
	var urls []string
	for _, m := range urlPattern.FindAllString(text, -1) {
		url := strings.TrimRight(m, trailingPunct)
		if url == "" {
			continue
		}
		if _, dup := seen[url]; dup {
			continue
		}
		seen[url] = struct{}{}
		urls = append(urls, url)
	}
	return urls
}
