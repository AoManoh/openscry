// Package sources separates a Grok answer from its cited sources and
// normalizes heterogeneous upstream citation formats into one consistent
// list. grok2api emits citations in several shapes depending on version and
// model: a trailing "## Sources" heading block, a <details> block, a function
// call like sources([...]), a tail block of bare link lines, inline
// [[N]](url) markers scattered through the body, or, failing all of those,
// ordinary [text](url) Markdown links inside the body.
//
// Split tries each strategy in priority order and returns the answer with the
// trailing sources block removed (inline citations and body links are kept in
// place, since they are part of the body's meaning) plus the extracted
// sources. This is a faithful Go port of GrokSearch's sources.py, kept
// transport-agnostic so the CLI and the MCP adapter share one extraction path.
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
	// Origin 标注来源出处：空值表示来自 Grok 答案本身（正文内联引用 / 尾部列表 / 上游
	// url_citation 注解）；"tavily" / "firecrawl" 表示由 extra_sources 的参考检索补充。
	// 暴露它是为了让调用方与评测能区分"模型引用了什么"与"旁路检索补了什么"。
	Origin string `json:"origin,omitempty"`
}

var (
	urlPattern      = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `，。、；：！？》）】)]+`)
	mdLinkPattern   = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)]+)\)`)
	headingPattern  = regexp.MustCompile(`(?im)^(?:#{1,6}\s*)?(?:\*\*|__)?\s*(?:sources?|references?|citations?|信源|参考资料|参考|引用|来源列表|来源)\s*[:：]?\s*(?:\*\*|__)?(?:\s*[（(][^)\n]*[)）])?\s*[:：]?\s*$`)
	functionPattern = regexp.MustCompile(`(?im)(^|\n)\s*(sources|source|citations|citation|references|reference|citation_card|source_cards|source_card)\s*\(`)
	inlineCitation  = regexp.MustCompile(`\[\[(\d+)\]\]\((https?://[^)\s]+)\)`)
	listPrefix      = regexp.MustCompile(`^\s*(?:[-*]|\d+\.)\s*`)
	trailingPunct   = ".,;:!?"

	// 以下几项只服务于 isLinkOnlyLine 的整行判定，与上面的提取用正则分开维护：提取阶段
	// 要尽量多捞 URL，判定阶段却必须整行精确，二者宽松程度不同，不能共用一个模式。
	//
	// Markdown 链接 token：链接文本沿用 mdLinkPattern 的形态（不含 "]"），URL 内不允许
	// 空白，只允许一层成对括号（维基百科条目常见的 "_(programming_language)"）。
	mdLinkToken = `\[[^\]]+\]\(https?://(?:[^()\s]|\([^()\s]*\))+\)`
	// 裸 URL token 只接受 RFC 3986 的 ASCII URL 字符（不含引号）。故意不接受任何非 ASCII
	// 字符：中文正文紧贴在 URL 之后而没有空格时会终止 token，整行随之落入"非链接行"。
	// 权衡：宁可漏识别带原生 Unicode 路径的 IRI 列表，也不冒删除正文的风险。
	bareURLToken = `https?://[A-Za-z0-9\-._~:/?#\[\]@!$&()*+,;=%]+`
	// 整行判定：去掉列表前缀后，必须是一个或多个链接 token；token 之间只允许空白与
	// 中英文逗号、分号、顿号，行尾只允许空白与中英文句末标点。出现任何其它字符即为正文。
	linkOnlyLine = regexp.MustCompile(`^(?:` + mdLinkToken + `|` + bareURLToken + `)(?:[\s,;、，；]*(?:` + mdLinkToken + `|` + bareURLToken + `))*[\s.,;:!?。，、；：！？]*$`)
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
		splitBodyMarkdownLinks,
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

// splitBodyMarkdownLinks 是策略链的兜底：前面所有策略都未命中时，把正文里的普通
// Markdown 链接 [文本](url) 收集为来源。为什么需要它：句尾带引用链接的正文不再被当成
// 尾部链接块之后，这类答案若不产出来源，搜索层会报"检索已执行但答案没有可解析引用"，
// 与答案明明带着链接的事实矛盾。正文原样返回，因为这些链接和 [[n]](url) 一样是句子
// 的一部分。[[n]](url) 不会重复处理：只要存在一个，splitInlineCitations 就已命中返回，
// 而且 mdLinkPattern 的链接文本不含 "]"，本身也匹配不上 "]]" 形态。
func splitBodyMarkdownLinks(text string) (string, []Source, bool) {
	src := extractMarkdownLinks(text)
	if len(src) == 0 {
		return "", nil, false
	}
	return text, src, true
}

// isLinkOnlyLine 判定一行是否只由链接组成。必须整行匹配而不是"行中含有链接即算"：
// 正文句子常以引用链接收尾（"……尚未获官方确认。[官方公告](url)"），若按含有链接判定，
// splitTailLinkBlock 会把这些带限制条件的正文当成来源列表整段删掉。
func isLinkOnlyLine(line string) bool {
	return linkOnlyLine.MatchString(strings.TrimSpace(listPrefix.ReplaceAllString(line, "")))
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

// extractMarkdownLinks 按出现顺序收集 [文本](url) 链接并按 URL 去重，链接文本作为
// Title。单独抽出来是为了让来源块提取与正文链接兜底共用同一套匹配与去重规则，
// 避免两处对"什么算 Markdown 链接"各自演化。
func extractMarkdownLinks(text string) []Source {
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
		src = append(src, Source{Title: strings.TrimSpace(m[1]), URL: url})
	}
	return src
}

// extractSourcesFromText scrapes markdown links first (preserving titles),
// then any remaining bare URLs, deduplicating by URL.
func extractSourcesFromText(text string) []Source {
	src := extractMarkdownLinks(text)
	seen := make(map[string]struct{}, len(src))
	for _, s := range src {
		seen[s.URL] = struct{}{}
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
