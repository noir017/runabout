package fleet

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Since 的游标是单调递增的绝对写入偏移。输出被截断后，
// 用快照字符串的下标当游标会指向错误的字节。
func TestCapWriterSinceTracksAbsoluteOffset(t *testing.T) {
	w := NewCapWriter(20)
	w.Write([]byte(strings.Repeat("a", 30)))

	if text, skipped, next := w.Since(0); text != "aaaaaaaaaa\n…（中间省略 10 字节，共 30 字节）…\naaaaaaaaaa" || skipped != 10 || next != 30 {
		t.Fatalf("从头读: text=%q skipped=%d next=%d", text, skipped, next)
	}
	// 游标在 head 内：返回 head 的剩余部分和 tail，并报告缺口。
	if text, skipped, next := w.Since(10); !strings.HasPrefix(text, "…（中间省略 10 字节") || skipped != 10 || next != 30 {
		t.Fatalf("游标在缺口起点: text=%q skipped=%d next=%d", text, skipped, next)
	}
	// 游标落在缺口内：缺口按游标之后的字节数算。
	if text, skipped, _ := w.Since(15); !strings.HasPrefix(text, "…（中间省略 5 字节") || skipped != 5 {
		t.Fatalf("游标在缺口内: text=%q skipped=%d", text, skipped)
	}
	// 游标在 tail 内：没有丢失，按偏移切片。
	if text, skipped, _ := w.Since(25); text != "aaaaa" || skipped != 0 {
		t.Fatalf("游标在 tail 内: text=%q skipped=%d", text, skipped)
	}
	if text, skipped, next := w.Since(30); text != "" || skipped != 0 || next != 30 {
		t.Fatalf("游标在末尾: text=%q skipped=%d next=%d", text, skipped, next)
	}
}

// 截断点落在多字节字符中间时，输出必须是合法 UTF-8。
func TestCapWriterSinceKeepsUTF8Boundaries(t *testing.T) {
	// head 取前 10 字节，正好切在"界"（第 9-11 字节）中间。
	w := NewCapWriter(20)
	w.Write([]byte("你好世界" + strings.Repeat("x", 20)))
	text, skipped, _ := w.Since(0)
	if !utf8.ValidString(text) {
		t.Fatalf("输出必须是合法 UTF-8: %q", text)
	}
	if !strings.HasPrefix(text, "你好世") {
		t.Fatalf("head 末尾的半个字符应整段丢掉: %q", text)
	}
	if !strings.Contains(text, "…（中间省略 13 字节，共 32 字节）…") {
		t.Fatalf("省略标记应完整且字节数正确: %q", text)
	}
	if skipped != 13 {
		t.Fatalf("skipped=%d，期望 13", skipped)
	}

	// tail 从 [17,27) 开始，起点在"世"（第 16-18 字节）中间。
	w2 := NewCapWriter(20)
	w2.Write([]byte(strings.Repeat("a", 10) + "你好世界" + strings.Repeat("b", 5)))
	text, skipped, _ = w2.Since(0)
	if !utf8.ValidString(text) {
		t.Fatalf("输出必须是合法 UTF-8: %q", text)
	}
	if !strings.HasSuffix(text, "界"+strings.Repeat("b", 5)) {
		t.Fatalf("tail 开头不完整的字符应丢掉: %q", text)
	}
	if skipped != 9 {
		t.Fatalf("skipped=%d，期望 9", skipped)
	}
}
