package app

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithRequestLogWarnsOnErrorAndPostCancel(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// 1. 正常 200 不打 WARN
	h200 := withRequestLog(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h200.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if buf.Len() != 0 {
		t.Fatalf("正常 200 不应输出 INFO/WARN 日志，实际: %s", buf.String())
	}

	// 2. 401/500 打 WARN
	h401 := withRequestLog(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	h401.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "status=401") {
		t.Fatalf("期望 401 打 WARN，实际: %s", buf.String())
	}

	// 3. POST 请求被客户端取消打 WARN
	buf.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	hCancel := withRequestLog(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
	}))
	hCancel.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx))
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "http 请求中止") {
		t.Fatalf("期望 POST 中止打 WARN，实际: %s", buf.String())
	}

	// 4. GET SSE 流被客户端关闭不打 WARN（避免刷屏）
	buf.Reset()
	ctxGet, cancelGet := context.WithCancel(context.Background())
	hGetCancel := withRequestLog(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancelGet()
	}))
	hGetCancel.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctxGet))
	if buf.Len() != 0 {
		t.Fatalf("GET 正常关闭不应打 WARN，实际: %s", buf.String())
	}
}
