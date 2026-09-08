package sources

import "testing"

func urls(src []Source) []string {
	out := make([]string, len(src))
	for i, s := range src {
		out[i] = s.URL
	}
	return out
}

func TestSplitEmpty(t *testing.T) {
	answer, src := Split("")
	if answer != "" || src != nil {
		t.Fatalf("empty input: answer=%q src=%v", answer, src)
	}
}

func TestSplitNoSources(t *testing.T) {
	answer, src := Split("Just a plain answer with no citations.")
	if answer != "Just a plain answer with no citations." {
		t.Fatalf("answer mutated: %q", answer)
	}
	if src != nil {
		t.Fatalf("expected nil sources, got %v", src)
	}
}

func TestSplitHeadingBlock(t *testing.T) {
	text := "The answer body.\n\n## Sources\n- [Go GC](https://go.dev/doc/gc-guide)\n- https://example.com/ref"
	answer, src := Split(text)
	if answer != "The answer body." {
		t.Fatalf("answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[0] != "https://go.dev/doc/gc-guide" || got[1] != "https://example.com/ref" {
		t.Fatalf("sources=%v", got)
	}
	if src[0].Title != "Go GC" {
		t.Fatalf("title=%q", src[0].Title)
	}
}

func TestSplitChineseHeading(t *testing.T) {
	text := "答案正文。\n\n参考资料：\n- [文档](https://go.dev/doc)"
	answer, src := Split(text)
	if answer != "答案正文。" {
		t.Fatalf("answer=%q", answer)
	}
	if len(src) != 1 || src[0].URL != "https://go.dev/doc" {
		t.Fatalf("sources=%v", urls(src))
	}
}

func TestSplitInlineCitations(t *testing.T) {
	text := "Go uses concurrent GC [[1]](https://go.dev/doc/gc-guide) with tricolor marking [[2]](https://draven.co/golang)."
	answer, src := Split(text)
	// Inline citations keep the full answer body.
	if answer != text {
		t.Fatalf("inline answer should be unchanged, got %q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[0] != "https://go.dev/doc/gc-guide" || got[1] != "https://draven.co/golang" {
		t.Fatalf("sources=%v", got)
	}
}

func TestSplitTailLinkBlock(t *testing.T) {
	text := "Body paragraph one.\n\nhttps://a.example.com\nhttps://b.example.com"
	answer, src := Split(text)
	if answer != "Body paragraph one." {
		t.Fatalf("answer=%q", answer)
	}
	if len(src) != 2 {
		t.Fatalf("sources=%v", urls(src))
	}
}

func TestSplitTailSingleLinkNotStripped(t *testing.T) {
	// A single trailing link is not a sources block (needs >=2).
	text := "See more at https://only-one.example.com"
	answer, src := Split(text)
	if answer != text || src != nil {
		t.Fatalf("single tail link should not split: answer=%q src=%v", answer, urls(src))
	}
}

// assertOfficialLinks 断言复现输入里的两条正文链接被收集为带标题的来源。
func assertOfficialLinks(t *testing.T, src []Source) {
	t.Helper()
	got := urls(src)
	if len(got) != 2 || got[0] != "https://official.example/announcement" || got[1] != "https://official.example/pricing" {
		t.Fatalf("sources=%v", got)
	}
	if src[0].Title != "官方公告" || src[1].Title != "价格页" {
		t.Fatalf("titles=%q,%q", src[0].Title, src[1].Title)
	}
}

// 回归：正文句子以 Markdown 链接结尾时，不能被 splitTailLinkBlock 当成来源列表切掉。
// 该输入不命中尾部链接块之前的任何策略，也没有 [[n]](url)，因此落到兜底策略
// splitBodyMarkdownLinks：answer 原样保留，正文里的两条链接按出现顺序收集为带标题的来源。
// （批次 A 首版曾断言 Sources 为 nil；追加要求后改为收集正文链接，否则搜索层会对带链接的
// 答案误报"没有可解析引用"。）
func TestSplitTailKeepsSentencesEndingWithLink(t *testing.T) {
	text := "产品信息摘要。\n\n屏幕尺寸尚未获官方确认。[官方公告](https://official.example/announcement)\n价格未公布，不应作为采购依据。[价格页](https://official.example/pricing)"
	answer, src := Split(text)
	if answer != text {
		t.Fatalf("body sentences were stripped: answer=%q", answer)
	}
	assertOfficialLinks(t, src)
}

// 回归：答案只有两行"正文 + 链接"时，旧实现会把全部正文删光，answer 变为空串。
// 来源断言同上：由兜底策略收集正文链接，而不是返回 nil。
func TestSplitTwoSentencesWithTrailingLinksNotEmptied(t *testing.T) {
	text := "屏幕尺寸尚未获官方确认。[官方公告](https://official.example/announcement)\n价格未公布，不应作为采购依据。[价格页](https://official.example/pricing)"
	answer, src := Split(text)
	if answer == "" {
		t.Fatal("answer must not be emptied")
	}
	if answer != text {
		t.Fatalf("answer=%q", answer)
	}
	assertOfficialLinks(t, src)
}

// 保护既有正确行为：纯 Markdown 链接列表仍被识别为来源块并从 answer 切掉。
func TestSplitTailMarkdownLinkList(t *testing.T) {
	text := "正文。\n\n- [标题一](https://a.example/x)\n- [标题二](https://b.example/y)"
	answer, src := Split(text)
	if answer != "正文。" {
		t.Fatalf("answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[0] != "https://a.example/x" || got[1] != "https://b.example/y" {
		t.Fatalf("sources=%v", got)
	}
	if src[0].Title != "标题一" || src[1].Title != "标题二" {
		t.Fatalf("titles=%q,%q", src[0].Title, src[1].Title)
	}
}

// 保护既有正确行为：带编号前缀的裸 URL 列表仍被识别为来源块。
func TestSplitTailNumberedBareURLList(t *testing.T) {
	text := "正文。\n\n1. https://a.example/x\n2. https://b.example/y"
	answer, src := Split(text)
	if answer != "正文。" {
		t.Fatalf("answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[0] != "https://a.example/x" || got[1] != "https://b.example/y" {
		t.Fatalf("sources=%v", got)
	}
}

// 尾随标点的判定：链接后只跟中英文句末标点时，该行没有正文可丢，仍视为链接行并切掉。
func TestSplitTailLinkListWithTrailingPunctuation(t *testing.T) {
	text := "正文。\n\n[标题一](https://a.example/x)。\n[标题二](https://b.example/y)."
	answer, src := Split(text)
	if answer != "正文。" {
		t.Fatalf("answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[0] != "https://a.example/x" || got[1] != "https://b.example/y" {
		t.Fatalf("sources=%v", got)
	}
}

// 回归：链接后带说明文字的列表项不再被当作链接行，整段保留在 answer 中；
// 其中的链接改由兜底策略 splitBodyMarkdownLinks 收集为带标题的来源（首版曾断言 nil）。
func TestSplitTailMixedLinkAndTextLinesKept(t *testing.T) {
	text := "正文。\n\n- [标题一](https://a.example/x) — 说明文字\n- [标题二](https://b.example/y) — 更多说明"
	answer, src := Split(text)
	if answer != text {
		t.Fatalf("mixed lines were stripped: answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[0] != "https://a.example/x" || got[1] != "https://b.example/y" {
		t.Fatalf("sources=%v", got)
	}
	if src[0].Title != "标题一" || src[1].Title != "标题二" {
		t.Fatalf("titles=%q,%q", src[0].Title, src[1].Title)
	}
}

// 兜底策略只在前面所有策略都未命中时运行：答案已含 [[n]](url) 时由 splitInlineCitations
// 收尾，同一 URL 再以普通 Markdown 链接出现也不会被重复收集。
func TestSplitInlineCitationsNotDuplicatedByBodyLinks(t *testing.T) {
	text := "Go 使用并发 GC [[1]](https://go.dev/doc/gc-guide)，细节见 [GC 指南](https://go.dev/doc/gc-guide)。"
	answer, src := Split(text)
	if answer != text {
		t.Fatalf("answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 1 || got[0] != "https://go.dev/doc/gc-guide" {
		t.Fatalf("sources=%v", got)
	}
}

// 兜底策略按出现顺序收集并按 URL 去重，重复链接保留首次出现的标题。
func TestSplitBodyMarkdownLinksDedupeByURL(t *testing.T) {
	text := "先看 [首次标题](https://a.example/x)，再看 [另一个](https://b.example/y)，最后回到 [重复标题](https://a.example/x)。"
	answer, src := Split(text)
	if answer != text {
		t.Fatalf("answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[0] != "https://a.example/x" || got[1] != "https://b.example/y" {
		t.Fatalf("sources=%v", got)
	}
	if src[0].Title != "首次标题" || src[1].Title != "另一个" {
		t.Fatalf("titles=%q,%q", src[0].Title, src[1].Title)
	}
}

// 没有任何 http(s) Markdown 链接时，Split 必须与以前完全一致：原文、nil。
// 方括号、圆括号、非 http 协议的链接以及散落的裸 URL 都不能触发兜底策略。
func TestSplitBodyWithoutMarkdownLinksStaysNil(t *testing.T) {
	for _, text := range []string{
		"数组下标 [0] 与函数调用 f(x) 都不是链接。",
		"联系 [邮箱](mailto:dev@example.com) 或 [镜像](ftp://mirror.example/pub)。",
		"参见 https://only-one.example.com 与 [占位符] 以及 (https://in-parens.example)。",
	} {
		answer, src := Split(text)
		if answer != text || src != nil {
			t.Fatalf("text=%q: answer=%q src=%v", text, answer, urls(src))
		}
	}
}

// 与旧实现保持一致：[[n]](url) 单独成行不属于尾部链接块（"]]" 不满足 Markdown 链接
// 形态），仍交给 splitInlineCitations 处理，正文原样保留、来源照常收集。
func TestSplitTailBracketedCitationLinesLeftToInlineStrategy(t *testing.T) {
	text := "正文。\n\n[[1]](https://a.example/x)\n[[2]](https://b.example/y)"
	answer, src := Split(text)
	if answer != text {
		t.Fatalf("answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[0] != "https://a.example/x" || got[1] != "https://b.example/y" {
		t.Fatalf("sources=%v", got)
	}
}

// 单行判定的边界表：只有整行由链接 token 组成（允许列表前缀、分隔符、行尾标点）才算链接行。
func TestIsLinkOnlyLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"bare url", "https://a.example/x", true},
		{"bare url with list prefix", "- https://a.example/x", true},
		{"bare url with numbered prefix", "1. https://a.example/x", true},
		{"bare url with parens", "https://en.wikipedia.org/wiki/Go_(programming_language)", true},
		{"markdown link", "[标题](https://a.example/x)", true},
		{"markdown link with list prefix", "- [标题](https://a.example/x)", true},
		{"markdown link with cjk trailing period", "[标题](https://a.example/x)。", true},
		{"markdown link with ascii trailing period", "[标题](https://a.example/x).", true},
		{"markdown link with paren url", "[Go](https://en.wikipedia.org/wiki/Go_(programming_language))", true},
		{"bracketed citation keeps old verdict", "[[1]](https://a.example/x)", false},
		{"two markdown links comma separated", "[a](https://a.example/x), [b](https://b.example/y)", true},
		{"two markdown links adjacent", "[a](https://a.example/x)[b](https://b.example/y)", true},
		{"two bare urls cjk comma separated", "https://a.example/x、https://b.example/y", true},
		{"markdown link then bare url", "[a](https://a.example/x) https://b.example/y", true},
		{"empty", "", false},
		{"list prefix only", "- ", false},
		{"text before link", "屏幕尺寸尚未获官方确认。[官方公告](https://official.example/announcement)", false},
		{"text after link", "[官方公告](https://official.example/announcement) 价格未公布", false},
		{"mixed list item", "- [标题](https://a.example/x) — 说明文字", false},
		{"text before bare url", "See more at https://only-one.example.com", false},
		{"bare url followed by cjk comma and text", "https://a.example/x，价格未公布", false},
		{"bare url glued to cjk text", "https://a.example/x官方公告", false},
		{"markdown link with space in url", "[标题](https://a.example/x y)", false},
		{"angle bracket autolink", "<https://a.example/x>", false},
		{"not http scheme", "ftp://a.example/x", false},
	}
	for _, tc := range cases {
		if got := isLinkOnlyLine(tc.line); got != tc.want {
			t.Errorf("%s: isLinkOnlyLine(%q)=%v want %v", tc.name, tc.line, got, tc.want)
		}
	}
}

func TestSplitDetailsBlock(t *testing.T) {
	text := "Answer text.\n\n<details><summary>Sources</summary>\n\n- [One](https://one.example.com)\n- [Two](https://two.example.com)\n</details>"
	answer, src := Split(text)
	if answer != "Answer text." {
		t.Fatalf("answer=%q", answer)
	}
	if len(src) != 2 {
		t.Fatalf("sources=%v", urls(src))
	}
}

func TestSplitFunctionCallJSON(t *testing.T) {
	text := `Here is the answer.

sources([{"title": "Go GC", "url": "https://go.dev/doc/gc-guide"}, {"url": "https://example.com/x"}])`
	answer, src := Split(text)
	if answer != "Here is the answer." {
		t.Fatalf("answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[0] != "https://go.dev/doc/gc-guide" {
		t.Fatalf("sources=%v", got)
	}
	if src[0].Title != "Go GC" {
		t.Fatalf("title=%q", src[0].Title)
	}
}

func TestSplitFunctionCallPairs(t *testing.T) {
	text := `Answer.

citations([["Title A", "https://a.example.com"], ["Title B", "https://b.example.com"]])`
	answer, src := Split(text)
	if answer != "Answer." {
		t.Fatalf("answer=%q", answer)
	}
	got := urls(src)
	if len(got) != 2 || got[1] != "https://b.example.com" {
		t.Fatalf("sources=%v", got)
	}
}

func TestMergeDeduplicates(t *testing.T) {
	a := []Source{{URL: "https://x.com"}, {URL: "https://y.com"}}
	b := []Source{{URL: "https://y.com"}, {URL: "https://z.com"}}
	merged := Merge(a, b)
	got := urls(merged)
	if len(got) != 3 {
		t.Fatalf("merged=%v want 3 unique", got)
	}
	if got[0] != "https://x.com" || got[2] != "https://z.com" {
		t.Fatalf("order not preserved: %v", got)
	}
}

func TestMergeSkipsEmptyURL(t *testing.T) {
	merged := Merge([]Source{{Title: "no url"}, {URL: "  "}, {URL: "https://ok.com"}})
	if len(merged) != 1 || merged[0].URL != "https://ok.com" {
		t.Fatalf("merged=%v", urls(merged))
	}
}
