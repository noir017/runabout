package tools

import (
	"fmt"
	"sync"
)

// capWriter 是带上限的输出收集器：超过上限后保留开头一半与末尾一半，
// 中间省略。命令输出往往"开头有报错、结尾有结论"，两端都不能丢。
type capWriter struct {
	limit int

	mu    sync.Mutex
	head  []byte
	tail  []byte
	total int
}

func newCapWriter(limit int) *capWriter {
	if limit <= 0 {
		limit = 200_000
	}
	return &capWriter{limit: limit}
}

func (w *capWriter) Write(p []byte) (int, error) {
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

// String 返回收集到的文本，必要时插入省略标记。
func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.tail) == 0 {
		return string(w.head)
	}
	omitted := w.total - len(w.head) - len(w.tail)
	if omitted <= 0 {
		return string(w.head) + string(w.tail)
	}
	return fmt.Sprintf("%s\n…（中间省略 %d 字节，共 %d 字节）…\n%s",
		w.head, omitted, w.total, w.tail)
}

// Total 返回写入的总字节数。
func (w *capWriter) Total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// Truncated 报告是否发生了截断。
func (w *capWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total > len(w.head)+len(w.tail)
}
