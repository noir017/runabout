package media

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeClock 是并发安全的手动时钟，后台清理协程也会读它。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestStore(t *testing.T, opts Options) (*Store, *fakeClock) {
	t.Helper()
	clk := newClock()
	s := newStore(opts, clk.Now)
	t.Cleanup(s.Close)
	return s, clk
}

// stats 在锁内读内部状态，避免与后台清理协程竞争。
func stats(s *Store) (items, listLen int, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items), s.order.Len(), s.total
}

func mustPut(t *testing.T, s *Store, data []byte, mime string) string {
	t.Helper()
	tok, _, err := s.Put(data, mime)
	if err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	return tok
}

func TestDefaults(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	if s.opts.TTL != 5*time.Minute || s.opts.MaxItems != 32 || s.opts.MaxTotalBytes != 64<<20 || s.opts.OneTime {
		t.Errorf("默认值不对: %+v", s.opts)
	}
}

func TestPutGetCopiesAndToken(t *testing.T) {
	s, clk := newTestStore(t, Options{})
	src := []byte("hello-image")
	tok, exp, err := s.Put(src, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 32 {
		t.Errorf("token 应为 32 字节随机数的 RawURL base64，实际 %q (%d 字节, %v)", tok, len(raw), err)
	}
	if want := clk.Now().Add(5 * time.Minute); !exp.Equal(want) {
		t.Errorf("过期时间应为 now+TTL，实际 %v", exp)
	}
	src[0] = 'X' // 调用方改原切片不能影响仓库里的内容
	data, mime, ok := s.Get(tok)
	if !ok || mime != "image/png" || string(data) != "hello-image" {
		t.Errorf("Get 结果不对: ok=%v mime=%q data=%q", ok, mime, data)
	}
	if _, _, ok := s.Get(tok); !ok {
		t.Error("默认不是一次性的，第二次也应能取到")
	}
	if tok2 := mustPut(t, s, []byte("x"), "image/png"); tok2 == tok {
		t.Error("两次 Put 不应得到同一个 token")
	}
	if _, _, ok := s.Get("no-such-token"); ok {
		t.Error("未知 token 应取不到")
	}
}

func TestExpiry(t *testing.T) {
	s, clk := newTestStore(t, Options{TTL: time.Minute})
	tok := mustPut(t, s, []byte("abc"), "image/png")
	clk.Advance(59 * time.Second)
	if _, _, ok := s.Get(tok); !ok {
		t.Fatal("有效期内应能取到")
	}
	clk.Advance(time.Second)
	if _, _, ok := s.Get(tok); ok {
		t.Fatal("到期后应取不到")
	}
	if n, l, total := stats(s); n != 0 || l != 0 || total != 0 {
		t.Errorf("过期条目应被惰性删除: items=%d list=%d total=%d", n, l, total)
	}
}

func TestSweepRemovesExpired(t *testing.T) {
	s, clk := newTestStore(t, Options{TTL: time.Minute})
	mustPut(t, s, []byte("old"), "image/png")
	clk.Advance(30 * time.Second)
	fresh := mustPut(t, s, []byte("new"), "image/png")
	clk.Advance(45 * time.Second)

	s.mu.Lock()
	s.sweepLocked(s.now())
	n, total := len(s.items), s.total
	s.mu.Unlock()
	if n != 1 || total != 3 {
		t.Errorf("清理后应只剩未过期的一条: items=%d total=%d", n, total)
	}
	if _, _, ok := s.Get(fresh); !ok {
		t.Error("未过期的条目不应被清理")
	}
}

func TestOneTime(t *testing.T) {
	s, _ := newTestStore(t, Options{OneTime: true})
	tok := mustPut(t, s, []byte("once"), "image/jpeg")
	if _, _, ok := s.peek(tok); !ok {
		t.Fatal("peek 应能看到")
	}
	if _, _, ok := s.Get(tok); !ok {
		t.Fatal("peek 不应消费一次性条目")
	}
	if _, _, ok := s.Get(tok); ok {
		t.Fatal("一次性条目读过一次后应删除")
	}
	if _, _, total := stats(s); total != 0 {
		t.Errorf("删除后总字节应归零，实际 %d", total)
	}
}

func TestEvictByItems(t *testing.T) {
	s, _ := newTestStore(t, Options{MaxItems: 2})
	a := mustPut(t, s, []byte("a"), "image/png")
	b := mustPut(t, s, []byte("b"), "image/png")
	c := mustPut(t, s, []byte("c"), "image/png")
	if _, _, ok := s.Get(a); ok {
		t.Error("超过条数上限时应淘汰最旧的")
	}
	for _, tok := range []string{b, c} {
		if _, _, ok := s.Get(tok); !ok {
			t.Error("较新的条目应保留")
		}
	}
}

func TestEvictByBytes(t *testing.T) {
	s, _ := newTestStore(t, Options{MaxTotalBytes: 10})
	a := mustPut(t, s, bytes.Repeat([]byte("a"), 4), "image/png")
	b := mustPut(t, s, bytes.Repeat([]byte("b"), 4), "image/png")
	c := mustPut(t, s, bytes.Repeat([]byte("c"), 4), "image/png")
	if _, _, ok := s.Get(a); ok {
		t.Error("超过总字节上限时应淘汰最旧的")
	}
	if _, _, ok := s.Get(b); !ok {
		t.Error("腾出空间后 b 应保留")
	}
	if _, _, ok := s.Get(c); !ok {
		t.Error("新条目应写入")
	}
	if _, _, total := stats(s); total != 8 {
		t.Errorf("总字节应为 8，实际 %d", total)
	}

	// 恰好等于上限可以放下，会把其他条目全部挤掉。
	full := mustPut(t, s, bytes.Repeat([]byte("f"), 10), "image/png")
	if _, _, ok := s.Get(full); !ok {
		t.Error("恰好等于上限的条目应能放下")
	}
	if n, _, _ := stats(s); n != 1 {
		t.Errorf("恰好等于上限的条目应独占仓库，items=%d", n)
	}
}

func TestPutTooLarge(t *testing.T) {
	s, _ := newTestStore(t, Options{MaxTotalBytes: 10})
	keep := mustPut(t, s, []byte("keep"), "image/png")
	_, _, err := s.Put(bytes.Repeat([]byte("x"), 11), "image/png")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("单条超过总容量应返回 ErrTooLarge，实际 %v", err)
	}
	if _, _, ok := s.Get(keep); !ok {
		t.Error("放不下的条目不应把已有内容挤掉")
	}
}

func TestCloseIdempotent(t *testing.T) {
	s := New(Options{TTL: time.Second})
	mustPut(t, s, []byte("x"), "image/png")
	s.Close()
	s.Close()
	if n, _, total := stats(s); n != 0 || total != 0 {
		t.Error("Close 后应释放全部内容")
	}
}

// 真实时钟下后台协程会按周期清理过期内容。
func TestJanitorRuns(t *testing.T) {
	s := New(Options{TTL: 10 * time.Millisecond})
	defer s.Close()
	mustPut(t, s, []byte("x"), "image/png")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _, _ := stats(s); n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("后台清理没有删除过期条目")
}

func TestSweepInterval(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		time.Millisecond: time.Second,
		10 * time.Second: 5 * time.Second,
		5 * time.Minute:  time.Minute,
	}
	for ttl, want := range cases {
		if got := sweepInterval(ttl); got != want {
			t.Errorf("sweepInterval(%v)=%v，期望 %v", ttl, got, want)
		}
	}
}

func serve(s *Store, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func checkSafetyHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control 应为 no-store，实际 %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options 应为 nosniff，实际 %q", got)
	}
}

func TestHandlerGet(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	body := []byte("\x89PNG-fake-bytes")
	tok := mustPut(t, s, body, "image/png")

	rec := serve(s, http.MethodGet, PathPrefix+tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 应返回 200，实际 %d", rec.Code)
	}
	checkSafetyHeaders(t, rec)
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type 应为 image/png，实际 %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length 不对: %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Error("响应体应与存入内容一致")
	}
}

func TestHandlerHeadDoesNotConsumeOneTime(t *testing.T) {
	s, _ := newTestStore(t, Options{OneTime: true})
	tok := mustPut(t, s, []byte("abcdef"), "image/jpeg")

	rec := serve(s, http.MethodHead, PathPrefix+tok)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD 应返回 200 且无响应体，实际 %d / %d 字节", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") != "6" || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("HEAD 也应带上 Content-Length/Content-Type: %v", rec.Header())
	}
	if rec := serve(s, http.MethodGet, PathPrefix+tok); rec.Code != http.StatusOK {
		t.Fatalf("HEAD 之后 GET 仍应成功，实际 %d", rec.Code)
	}
	if rec := serve(s, http.MethodGet, PathPrefix+tok); rec.Code != http.StatusNotFound {
		t.Errorf("一次性链接第二次 GET 应 404，实际 %d", rec.Code)
	}
}

func TestHandlerNotFoundAndExpired(t *testing.T) {
	s, clk := newTestStore(t, Options{TTL: time.Minute})
	tok := mustPut(t, s, []byte("x"), "image/png")

	for _, p := range []string{PathPrefix, PathPrefix + "nope", PathPrefix + tok + "/extra"} {
		rec := serve(s, http.MethodGet, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 应 404，实际 %d", p, rec.Code)
		}
		checkSafetyHeaders(t, rec)
	}
	clk.Advance(time.Minute)
	if rec := serve(s, http.MethodGet, PathPrefix+tok); rec.Code != http.StatusNotFound {
		t.Errorf("过期链接应 404，实际 %d", rec.Code)
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	tok := mustPut(t, s, []byte("x"), "image/png")
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := serve(s, m, PathPrefix+tok)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 应返回 405，实际 %d", m, rec.Code)
		}
		if rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("405 应带 Allow 头，实际 %q", rec.Header().Get("Allow"))
		}
	}
	if _, _, ok := s.Get(tok); !ok {
		t.Error("被拒绝的方法不应影响内容")
	}
}
