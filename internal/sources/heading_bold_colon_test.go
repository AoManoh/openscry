package sources

import "testing"

// 模型常把冒号写在加粗内部（**来源：**）；该行也必须被识别为来源标题并从正文剥离。
func TestSplitHeadingWithColonInsideBold(t *testing.T) {
	text := "正文。[[1]](https://a.example/x)\n\n**来源：**\n1. https://a.example/x\n2. https://b.example/y\n"
	answer, src := Split(text)
	if answer != "正文。[[1]](https://a.example/x)" {
		t.Fatalf("heading line must be stripped, got answer %q", answer)
	}
	if len(src) != 2 || src[0].URL != "https://a.example/x" || src[1].URL != "https://b.example/y" {
		t.Fatalf("sources = %+v", src)
	}
	// 英文加粗标题同样处理。
	answer, src = Split("Body.[[1]](https://a.example/x)\n\n**Sources:**\n- https://a.example/x\n")
	if answer != "Body.[[1]](https://a.example/x)" || len(src) != 1 {
		t.Fatalf("english bold heading: answer=%q sources=%+v", answer, src)
	}
}
