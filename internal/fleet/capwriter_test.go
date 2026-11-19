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
	// 游标恰好落在缺口终点：从 tail 起点读，不报丢失也不重复。
	if text, skipped, next := w.Since(20); text != "aaaaaaaaaa" || skipped != 0 || next != 30 {
		t.Fatalf("游标在缺口终点: text=%q skipped=%d next=%d", text, skipped, next)
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

// 游标落在多字节字符中间时，向后跳到下一个完整字符，跳过的字节计入 skipped。
func TestCapWriterSinceAlignsCursorToRune(t *testing.T) {
	// 没有截断：Write 把"你"切成两次，游标停在续字节上。
	w := NewCapWriter(100)
	w.Write([]byte{0xE4, 0xBD})
	w.Write([]byte{0xA0, 'o', 'k'})
	text, skipped, next := w.Since(2)
	if !utf8.ValidString(text) {
		t.Fatalf("输出必须是合法 UTF-8: %q", text)
	}
	if !strings.HasSuffix(text, "ok") {
		t.Fatalf("应返回跳过续字节之后的内容: %q", text)
	}
	if !strings.Contains(text, "…（中间省略 1 字节，共 5 字节）…") || skipped != 1 || next != 5 {
		t.Fatalf("text=%q skipped=%d next=%d", text, skipped, next)
	}

	// 已截断且游标在 head 内：head = 10 个 a + "你"的前 2 字节。
	w2 := NewCapWriter(24)
	w2.Write([]byte(strings.Repeat("a", 10) + "你" + strings.Repeat("x", 20)))
	text, skipped, next = w2.Since(11)
	if !utf8.ValidString(text) {
		t.Fatalf("输出必须是合法 UTF-8: %q", text)
	}
	if !strings.HasSuffix(text, strings.Repeat("x", 12)) {
		t.Fatalf("tail 应完整保留: %q", text)
	}
	if skipped != 10 || next != 33 {
		t.Fatalf("text=%q skipped=%d next=%d", text, skipped, next)
	}

	// 已截断且游标在 tail 内：tail 从 [17,27)，游标 18 落在"世"中间。
	w3 := NewCapWriter(20)
	w3.Write([]byte(strings.Repeat("a", 10) + "你好世界" + strings.Repeat("b", 5)))
	text, skipped, next = w3.Since(18)
	if !utf8.ValidString(text) {
		t.Fatalf("输出必须是合法 UTF-8: %q", text)
	}
	if !strings.HasSuffix(text, "界"+strings.Repeat("b", 5)) {
		t.Fatalf("应返回下一个完整字符及其后的内容: %q", text)
	}
	if skipped != 1 || next != 27 {
		t.Fatalf("text=%q skipped=%d next=%d", text, skipped, next)
	}
}

// off 超过 total 时读取按末尾算，但游标不能回退，否则会重复交付。
func TestCapWriterSinceDoesNotRewindPastTotal(t *testing.T) {
	w := NewCapWriter(20)
	w.Write([]byte("abc"))

	text, skipped, next := w.Since(100)
	if text != "" || skipped != 0 {
		t.Fatalf("越界游标应读到空内容: text=%q skipped=%d", text, skipped)
	}
	if next != 100 {
		t.Fatalf("游标不能回退: next=%d", next)
	}
	// 后续写入不会把游标跨过的区间重新交付。
	w.Write([]byte("def"))
	if text, skipped, next = w.Since(100); text != "" || skipped != 0 || next != 100 {
		t.Fatalf("越界游标不应重复交付: text=%q skipped=%d next=%d", text, skipped, next)
	}
}
