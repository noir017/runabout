package fleet

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// CapWriter 是带上限的输出收集器：超过上限后保留开头一半与末尾一半，
// 中间省略。命令输出往往"开头有报错、结尾有结论"，两端都不能丢。
type CapWriter struct {
	limit int

	mu    sync.Mutex
	head  []byte
	tail  []byte
	total int
}

func NewCapWriter(limit int) *CapWriter {
	if limit <= 0 {
		limit = 200_000
	}
	return &CapWriter{limit: limit}
}

func (w *CapWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// 必须原样返回输入长度：io.Writer 的约定是 n < len(p) 即视为 short write，
	// exec 会据此判定命令启动失败。
	written := len(p)
	w.total += written
	headCap := w.limit / 2
	if len(w.head) < headCap {
		n := headCap - len(w.head)
		if n > len(p) {
			n = len(p)
		}
		w.head = append(w.head, p[:n]...)
		p = p[n:]
	}
	if len(p) == 0 {
		return written, nil
	}
	tailCap := w.limit - headCap
	w.tail = append(w.tail, p...)
	if len(w.tail) > tailCap {
		w.tail = w.tail[len(w.tail)-tailCap:]
	}
	return written, nil
}

// Since 返回绝对写入偏移 off 之后仍然保留的输出。
//
// off 是单调递增的总写入字节数，不能用 String() 的下标代替：截断后省略标记
// 的长度和 tail 的位置都会变化，字符串下标会指向错误的字节。
//
// 返回的 text 从 off 开始；如果这段内容已被头尾裁剪掉一部分，text 会从现存
// 最早的位置开始，并插入标记说明丢了多少字节。skipped 是丢失的字节数
// （0 表示没有丢），next 是下一次读取应传入的偏移。
func (w *CapWriter) Since(off int) (text string, skipped, next int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sinceLocked(off)
}

func (w *CapWriter) sinceLocked(off int) (string, int, int) {
	if off < 0 {
		off = 0
	}
	if off >= w.total {
		return "", 0, w.total
	}
	h := len(w.head)
	gapStart, gapEnd := h, w.total-len(w.tail)
	if gapStart >= gapEnd {
		// 没有截断：head+tail 连续覆盖整个流，下标就是绝对偏移。
		if off <= h {
			return string(w.head[off:]) + string(w.tail), 0, w.total
		}
		return string(w.tail[off-h:]), 0, w.total
	}

	var headPart, tailPart []byte
	skipped := 0
	if off < gapStart {
		// head 末尾可能切在多字节字符中间，半个字符无法还原，整段丢掉。
		end := off + completePrefixLen(w.head[off:])
		headPart = w.head[off:end]
		skipped = gapEnd - end
	} else if off < gapEnd {
		// 游标落在被省略的区间里，从现存最早的 tail 开始给。
		skipped = gapEnd - off
	}
	if off < gapEnd {
		// tail 开头可能是被裁掉字符的续字节，跳过。
		n := continuationPrefixLen(w.tail)
		tailPart = w.tail[n:]
		skipped += n
	} else {
		tailPart = w.tail[off-gapEnd:]
	}

	if skipped == 0 {
		return string(headPart) + string(tailPart), 0, w.total
	}
	var b strings.Builder
	b.Write(headPart)
	if len(headPart) > 0 {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "…（中间省略 %d 字节，共 %d 字节）…\n", skipped, w.total)
	b.Write(tailPart)
	return b.String(), skipped, w.total
}

// completePrefixLen 返回 b 中最后一个完整 UTF-8 字符的结束位置。
// 末尾如果是被截断的多字节字符，从它的起始字节起都算不完整。
func completePrefixLen(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if utf8.FullRune(b[i:]) {
			return len(b)
		}
		return i
	}
	return len(b)
}

// continuationPrefixLen 返回 b 开头连续 UTF-8 续字节的个数（最多 3 个）。
func continuationPrefixLen(b []byte) int {
	n := 0
	for n < len(b) && n < utf8.UTFMax-1 && !utf8.RuneStart(b[n]) {
		n++
	}
	return n
}

// String 返回收集到的文本，必要时插入省略标记。
func (w *CapWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	text, _, _ := w.sinceLocked(0)
	return text
}

// Total 返回写入的总字节数。
func (w *CapWriter) Total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// Truncated 报告是否发生了截断。
func (w *CapWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total > len(w.head)+len(w.tail)
}
