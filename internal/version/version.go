// Package version 是 openscry 的唯一版本事实源。
//
// 版本真相只来自 Go 工具链写入二进制的 build 信息：`go install / go run
// github.com/AoManoh/openscry/cmd/openscry@vX.Y.Z` 得到发布 tag，本地构建得到
// VCS 伪版本或"最近 tag+dirty"（工具链 1.24 起自动写入），两者都不需要 ldflags
// 或发布流水线。此前 MCP serverInfo、`openscry version` 与 get_config_info 共用
// 一个硬编码常量（0.2.0-sN），每次发版都要手改并与 tag 保持一致，本包用读取
// build 信息替代它。做法与同作者项目 openpe 的 internal/version 一致。
package version

import (
	"runtime/debug"
	"strings"
)

// Devel 是无可用模块版本时的归一值：非 module 构建（ReadBuildInfo 不可用）
// 或工具链标记的 "(devel)"（如 go test、禁用 buildvcs 的构建）。
const Devel = "devel"

// readBuildInfo 可在测试中替换，隔离对真实构建环境的依赖。
var readBuildInfo = debug.ReadBuildInfo

// Value 返回当前二进制的版本字符串：发布 tag（v0.2.0）、VCS 伪版本或
// "最近 tag+dirty"（v0.2.0-s10+dirty），否则为 Devel。所有消费点（`openscry version`、
// MCP serverInfo、get_config_info）都必须经由本函数。
func Value() string {
	info, ok := readBuildInfo()
	if !ok {
		return Devel
	}
	return Normalize(info.Main.Version)
}

// Normalize 把 BuildInfo.Main.Version 的原始值映射为用户可见值：空串与
// "(devel)" 归一为 Devel，其余（tag、伪版本，含 +dirty 后缀）原样保留。
func Normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "(devel)" {
		return Devel
	}
	return raw
}
