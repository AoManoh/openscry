package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRequiresModel(t *testing.T) {
	t.Setenv("GROK_API_URL", "https://example.test/v1")
	t.Setenv("GROK_API_KEY", "k-test")
	t.Setenv("GROK_MODEL", "") // empty = unset; must fail loud

	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail when GROK_MODEL is unset, got nil")
	}
	if !strings.Contains(err.Error(), "GROK_MODEL") {
		t.Fatalf("error should mention GROK_MODEL, got: %v", err)
	}
}

func TestLoadModelIsUserSupplied(t *testing.T) {
	t.Setenv("GROK_API_URL", "https://example.test/v1")
	t.Setenv("GROK_API_KEY", "k-test")
	t.Setenv("GROK_MODEL", "my-model")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "my-model" {
		t.Fatalf("Model = %q, want my-model", cfg.Model)
	}
	if cfg.FetchFallback != "full" {
		t.Fatalf("FetchFallback default = %q, want full", cfg.FetchFallback)
	}
}

func TestLoadFetchFallbackValidation(t *testing.T) {
	t.Setenv("GROK_API_URL", "https://example.test/v1")
	t.Setenv("GROK_API_KEY", "k-test")
	t.Setenv("GROK_MODEL", "my-model")

	t.Setenv("GROK_FETCH_FALLBACK", "strict")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error for strict: %v", err)
	}
	if cfg.FetchFallback != "strict" {
		t.Fatalf("FetchFallback = %q, want strict", cfg.FetchFallback)
	}

	t.Setenv("GROK_FETCH_FALLBACK", "bogus")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid GROK_FETCH_FALLBACK, got nil")
	}
}

func TestLoadMCPToolsValidation(t *testing.T) {
	t.Setenv("GROK_API_URL", "https://example.test/v1")
	t.Setenv("GROK_API_KEY", "k-test")
	t.Setenv("GROK_MODEL", "my-model")

	// Default when unset.
	t.Setenv("GROK_MCP_TOOLS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MCPTools != DefaultMCPTools {
		t.Fatalf("default MCPTools = %q, want %q", cfg.MCPTools, DefaultMCPTools)
	}

	// Explicit all (case-insensitive).
	t.Setenv("GROK_MCP_TOOLS", "ALL")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error for all: %v", err)
	}
	if cfg.MCPTools != "all" {
		t.Fatalf("MCPTools = %q, want all", cfg.MCPTools)
	}

	// Invalid value fails loud.
	t.Setenv("GROK_MCP_TOOLS", "bogus")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid GROK_MCP_TOOLS, got nil")
	}
}

func TestNormalizeToolset(t *testing.T) {
	for _, in := range []string{"core", "CORE", " all ", "All"} {
		if _, err := NormalizeToolset(in); err != nil {
			t.Errorf("NormalizeToolset(%q) unexpected error: %v", in, err)
		}
	}
	for _, in := range []string{"", "minimal", "everything"} {
		if _, err := NormalizeToolset(in); err == nil {
			t.Errorf("NormalizeToolset(%q) expected error, got nil", in)
		}
	}
}

func TestParseSearchTools(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{"web_search"}},           // 未设置 -> 默认
		{"   ", []string{"web_search"}},        // 空白 -> 默认
		{"none", []string{}},                   // 显式关闭
		{"OFF", []string{}},                    // 关闭同义词，大小写不敏感
		{"web_search", []string{"web_search"}}, // 单项
		{" Web_Search , x_search ,, web_search ", []string{"web_search", "x_search"}}, // trim/小写/去重/空项
	}
	for _, c := range cases {
		got, err := ParseSearchTools(c.in)
		if err != nil {
			t.Errorf("ParseSearchTools(%q) unexpected error: %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("ParseSearchTools(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ParseSearchTools(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
	// 非法字符与只剩空项的清单都必须 fail-loud，而不是静默变成"无工具"。
	for _, in := range []string{"web search", "web_search;x_search", `{"type":"web_search"}`, ",,,"} {
		if _, err := ParseSearchTools(in); err == nil {
			t.Errorf("ParseSearchTools(%q) expected error, got nil", in)
		}
	}
}

func TestLoadSearchTools(t *testing.T) {
	t.Setenv("GROK_API_URL", "https://example.test/v1")
	t.Setenv("GROK_API_KEY", "k-test")
	t.Setenv("GROK_MODEL", "my-model")

	// 未设置时默认声明 web_search。
	t.Setenv("GROK_SEARCH_TOOLS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.SearchTools) != 1 || cfg.SearchTools[0] != "web_search" {
		t.Fatalf("default SearchTools = %v, want [web_search]", cfg.SearchTools)
	}

	// none 关闭声明。
	t.Setenv("GROK_SEARCH_TOOLS", "none")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error for none: %v", err)
	}
	if len(cfg.SearchTools) != 0 {
		t.Fatalf("SearchTools for none = %v, want empty", cfg.SearchTools)
	}

	// 非法值启动即失败。
	t.Setenv("GROK_SEARCH_TOOLS", "web search")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid GROK_SEARCH_TOOLS, got nil")
	}
}

func TestLoadQueueWaitTimeout(t *testing.T) {
	t.Setenv("GROK_API_URL", "https://example.test/v1")
	t.Setenv("GROK_API_KEY", "k-test")
	t.Setenv("GROK_MODEL", "my-model")

	// 解析规则与 GROK_REQUEST_TIMEOUT 一致：Go 时长或裸秒数；0 是合法的“立即拒绝”；
	// 超过上限裁剪而不是报错，与 GROK_REQUEST_TIMEOUT 的钳制口径相同。
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultQueueWaitTimeout},             // 未设置 -> 默认 10s
		{"0", 0},                                  // 0 -> 立即拒绝
		{"0s", 0},                                 // Go 时长形式的 0
		{"3", 3 * time.Second},                    // 裸秒数
		{"2.5", 2500 * time.Millisecond},          // 裸秒数允许小数
		{"1m", time.Minute},                       // Go 时长
		{"20m", MaxQueueWaitTimeout},              // 超上限 -> 裁剪到 10m
		{" 15s ", 15 * time.Second},               // 首尾空白被忽略
		{"600", MaxQueueWaitTimeout},              // 裸秒数恰好等于上限
		{"601", MaxQueueWaitTimeout},              // 裸秒数超上限 -> 裁剪
		{"10ms", 10 * time.Millisecond},           // 小于 1s 的值不被抬高：没有下限，只有上限
		{"1h30m", MaxQueueWaitTimeout},            // 复合时长超上限 -> 裁剪
		{"0.5", 500 * time.Millisecond},           // 小数秒
		{"10s", DefaultQueueWaitTimeout},          // 显式写默认值
		{"600s", MaxQueueWaitTimeout},             // Go 时长恰好等于上限
		{"9m59s", 9*time.Minute + 59*time.Second}, // 上限以内保持原值
	}
	for _, c := range cases {
		t.Setenv("GROK_QUEUE_WAIT_TIMEOUT", c.raw)
		cfg, err := Load()
		if err != nil {
			t.Errorf("GROK_QUEUE_WAIT_TIMEOUT=%q: unexpected error: %v", c.raw, err)
			continue
		}
		if cfg.QueueWaitTimeout != c.want {
			t.Errorf("GROK_QUEUE_WAIT_TIMEOUT=%q: QueueWaitTimeout = %v, want %v", c.raw, cfg.QueueWaitTimeout, c.want)
		}
	}

	// 负值与无法解析的值都必须启动即失败，而不是静默回落默认值或钳到 0。
	for _, raw := range []string{"-1", "-1s", "-0.5", "abc", "10x", "1s2", "ten"} {
		t.Setenv("GROK_QUEUE_WAIT_TIMEOUT", raw)
		_, err := Load()
		if err == nil {
			t.Errorf("GROK_QUEUE_WAIT_TIMEOUT=%q: expected error, got nil", raw)
			continue
		}
		if !strings.Contains(err.Error(), "GROK_QUEUE_WAIT_TIMEOUT") {
			t.Errorf("GROK_QUEUE_WAIT_TIMEOUT=%q: error should name the variable, got: %v", raw, err)
		}
	}
}

func TestFetchToolsOnlyForwardsWebSearch(t *testing.T) {
	cases := []struct {
		tools []string
		want  []string
	}{
		{[]string{"web_search"}, []string{"web_search"}},
		{[]string{"x_search", "web_search"}, []string{"web_search"}}, // x_search 对抓取无意义，不透传
		{[]string{"x_search"}, nil},
		{[]string{}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		got := (&Config{SearchTools: c.tools}).FetchTools()
		if len(got) != len(c.want) || (len(got) == 1 && got[0] != c.want[0]) {
			t.Errorf("FetchTools(%v) = %v, want %v", c.tools, got, c.want)
		}
	}
}
