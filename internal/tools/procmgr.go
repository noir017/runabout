package tools

import (
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/noir017/agent-tools-mcp/internal/idgen"
)

// Proc 是一个后台运行的 shell 进程。
//
// HTTP 请求有超时，长任务（编译、迁移、压测）不能占着一次工具调用不放，
// 所以 shell 支持丢到后台，再用 shell_output / shell_kill 观察和收尾。
type Proc struct {
	ID      string
	Label   string
	Command string
	Workdir string
	Started time.Time

	cmd    *exec.Cmd
	out    *capWriter
	done   chan struct{}
	cursor int // shell_output 已读到的位置

	mu       sync.Mutex
	finished bool
	ended    time.Time
	exitCode int
	runErr   error
}

// ProcManager 管理后台进程集合。
type ProcManager struct {
	max int

	mu    sync.Mutex
	procs map[string]*Proc
}

func NewProcManager(max int) *ProcManager {
	if max <= 0 {
		max = 16
	}
	return &ProcManager{max: max, procs: map[string]*Proc{}}
}

// Add 登记一个新进程；超过上限时先清理已结束的，仍超限则报错。
func (m *ProcManager) Add(p *Proc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.procs) >= m.max {
		m.reapLocked()
	}
	if len(m.procs) >= m.max {
		return fmt.Errorf("后台进程已达上限 %d，请先用 shell_kill 结束一些，或等它们跑完", m.max)
	}
	m.procs[p.ID] = p
	return nil
}

// reapLocked 清掉结束超过 30 分钟的记录，避免无限堆积。
func (m *ProcManager) reapLocked() {
	cutoff := time.Now().Add(-30 * time.Minute)
	for id, p := range m.procs {
		p.mu.Lock()
		stale := p.finished && p.ended.Before(cutoff)
		p.mu.Unlock()
		if stale {
			delete(m.procs, id)
		}
	}
}

func (m *ProcManager) Get(id string) (*Proc, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[id]
	return p, ok
}

// List 返回按启动时间倒序排列的进程。
func (m *ProcManager) List() []*Proc {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Proc, 0, len(m.procs))
	for _, p := range m.procs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

func (m *ProcManager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.procs, id)
}

// KillAll 在服务退出时结束所有后台进程。
func (m *ProcManager) KillAll() {
	for _, p := range m.List() {
		_ = p.Signal(syscall.SIGTERM)
	}
}

func newProcID() string { return idgen.New("proc") }

// Running 报告进程是否仍在运行。
func (p *Proc) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.finished
}

// Status 返回状态快照。
func (p *Proc) Status() (running bool, exitCode int, runErr error, ended time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.finished, p.exitCode, p.runErr, p.ended
}

// markDone 记录退出状态。
func (p *Proc) markDone(exitCode int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finished = true
	p.ended = time.Now()
	p.exitCode = exitCode
	p.runErr = err
	close(p.done)
}

// Output 返回全部已捕获输出。
func (p *Proc) Output() string { return p.out.String() }

// Signal 给整个进程组发信号：后台任务常常自己再拉子进程，只杀父进程会留孤儿。
func (p *Proc) Signal(sig syscall.Signal) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return fmt.Errorf("进程尚未启动")
	}
	pgid := -p.cmd.Process.Pid
	if err := syscall.Kill(pgid, sig); err != nil {
		// 进程组可能已经不存在，退回到单进程。
		return p.cmd.Process.Signal(sig)
	}
	return nil
}

// Wait 等待进程结束或超时。
func (p *Proc) Wait(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
		return false
	}
}
