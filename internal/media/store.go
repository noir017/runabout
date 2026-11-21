// Package media 是一个进程内的临时媒体仓库：把一段字节放进内存，换回一个
// 高熵随机 token，在有效期内可经 HTTP 用 /media/<token> 取回。
//
// 它用于给无法直接展示 MCP 图片块的客户端提供短时效下载链接。下载端点
// 不走 OAuth：拿链接的一方（客户端侧的下载工具）通常没有 Bearer，安全性
// 完全依赖 256-bit 随机 token 与短 TTL，所以 token 不得写进日志。
package media

import (
	"container/list"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PathPrefix 是下载端点的路由前缀，完整路径为 PathPrefix + token。
const PathPrefix = "/media/"

const (
	DefaultTTL           = 5 * time.Minute
	DefaultMaxItems      = 32
	DefaultMaxTotalBytes = int64(64 << 20)

	tokenBytes = 32
)

// ErrTooLarge 表示单条内容超过了仓库总容量，无论怎么淘汰都放不下。
var ErrTooLarge = errors.New("内容超过临时仓库总容量")

// Options 是仓库配置；零值字段取默认值。
type Options struct {
	// TTL 是每条内容的有效期，默认 5m。
	TTL time.Duration
	// OneTime 为 true 时内容被 GET 一次即删除（HEAD 不算）。默认 false，只靠 TTL 过期。
	OneTime bool
	// MaxItems 是同时保留的条数上限，超出时淘汰最旧的，默认 32。
	MaxItems int
	// MaxTotalBytes 是所有内容的总字节上限，超出时淘汰最旧的，默认 64MB。
	MaxTotalBytes int64
}

func (o Options) withDefaults() Options {
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.MaxItems <= 0 {
		o.MaxItems = DefaultMaxItems
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = DefaultMaxTotalBytes
	}
	return o
}

type entry struct {
	token   string
	data    []byte
	mime    string
	expires time.Time
	elem    *list.Element
}

// Store 是并发安全的内存临时仓库。用 New 创建，用完调 Close 停掉后台清理。
type Store struct {
	opts Options
	now  func() time.Time

	mu    sync.Mutex
	items map[string]*entry
	order *list.List // 按写入先后排列，Front 最旧；TTL 统一，所以也是最早过期的
	total int64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// New 创建仓库并启动后台过期清理。
func New(opts Options) *Store {
	return newStore(opts, time.Now)
}

func newStore(opts Options, now func() time.Time) *Store {
	opts = opts.withDefaults()
	s := &Store{
		opts:  opts,
		now:   now,
		items: map[string]*entry{},
		order: list.New(),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.janitor(sweepInterval(opts.TTL))
	return s
}

// TTL 返回实际生效的有效期。
func (s *Store) TTL() time.Duration { return s.opts.TTL }

// OneTime 报告是否启用了读一次即删。
func (s *Store) OneTime() bool { return s.opts.OneTime }

// Put 保存一份 data 的拷贝，返回下载 token 与过期时间。
// 条数或总字节超限时先淘汰最旧的条目；单条超过 MaxTotalBytes 返回 ErrTooLarge。
func (s *Store) Put(data []byte, mime string) (string, time.Time, error) {
	size := int64(len(data))
	if size > s.opts.MaxTotalBytes {
		return "", time.Time{}, fmt.Errorf("%w（%d 字节 > %d 字节）", ErrTooLarge, size, s.opts.MaxTotalBytes)
	}
	token, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	e := &entry{token: token, data: append([]byte(nil), data...), mime: mime}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	for s.order.Len() > 0 && (s.order.Len() >= s.opts.MaxItems || s.total+size > s.opts.MaxTotalBytes) {
		s.removeLocked(s.order.Front().Value.(*entry))
	}
	e.expires = now.Add(s.opts.TTL)
	e.elem = s.order.PushBack(e)
	s.items[token] = e
	s.total += size
	return token, e.expires, nil
}

// Get 取回 token 对应的内容。过期的条目在这里惰性删除；OneTime 时读一次即删。
// 返回的切片由仓库持有，调用方不得修改。
func (s *Store) Get(token string) (data []byte, mime string, ok bool) {
	return s.get(token, s.opts.OneTime)
}

// peek 与 Get 相同，但永不消费 OneTime 条目（给 HEAD 用）。
func (s *Store) peek(token string) ([]byte, string, bool) {
	return s.get(token, false)
}

func (s *Store) get(token string, consume bool) ([]byte, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[token]
	if !ok {
		return nil, "", false
	}
	if !s.now().Before(e.expires) {
		s.removeLocked(e)
		return nil, "", false
	}
	if consume {
		s.removeLocked(e)
	}
	return e.data, e.mime, true
}

// Close 停止后台清理并释放全部内容。可重复调用。
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		s.items = map[string]*entry{}
		s.order.Init()
		s.total = 0
		s.mu.Unlock()
	})
}

// Handler 处理 GET/HEAD PathPrefix+token。缺失或过期一律 404，不区分两者。
func (s *Store) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := strings.TrimPrefix(r.URL.Path, PathPrefix)
		if token == "" || token == r.URL.Path || strings.Contains(token, "/") {
			http.NotFound(w, r)
			return
		}
		var (
			data []byte
			mime string
			ok   bool
		)
		if r.Method == http.MethodHead {
			data, mime, ok = s.peek(token)
		} else {
			data, mime, ok = s.Get(token)
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if mime == "" {
			mime = "application/octet-stream"
		}
		h.Set("Content-Type", mime)
		h.Set("Content-Length", strconv.Itoa(len(data)))
		// 内容只当作资源展示，不让浏览器把它当页面执行任何东西。
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	})
}

func (s *Store) janitor(every time.Duration) {
	defer close(s.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			s.sweepLocked(s.now())
			s.mu.Unlock()
		}
	}
}

// sweepLocked 从最旧的一端删掉所有已过期条目；TTL 统一，遇到未过期的即可停。
func (s *Store) sweepLocked(now time.Time) {
	for el := s.order.Front(); el != nil; el = s.order.Front() {
		e := el.Value.(*entry)
		if now.Before(e.expires) {
			return
		}
		s.removeLocked(e)
	}
}

func (s *Store) removeLocked(e *entry) {
	if _, ok := s.items[e.token]; !ok {
		return
	}
	delete(s.items, e.token)
	s.order.Remove(e.elem)
	s.total -= int64(len(e.data))
}

// sweepInterval 取 TTL 的一半，限定在 [1s, 1m]：既不空转，也不让过期内容在内存里滞留太久。
func sweepInterval(ttl time.Duration) time.Duration {
	return min(max(ttl/2, time.Second), time.Minute)
}

func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机 token 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
