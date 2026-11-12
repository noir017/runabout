package imaging

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVisionDescribe(t *testing.T) {
	var gotPath, gotAuth, gotModel, gotDataURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("请求不是合法 JSON: %v", err)
		}
		gotModel = req.Model
		for _, part := range req.Messages[0].Content {
			if part.Type == "image_url" {
				gotDataURL = part.ImageURL.URL
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"界面显示：登录失败，密码错误。"}}]}`))
	}))
	defer srv.Close()

	v := &Vision{BaseURL: srv.URL + "/v1/", Model: "vision-test", APIKey: "test-key", Timeout: time.Second}
	got, err := v.Describe(context.Background(), []byte("png-bytes"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "登录失败") {
		t.Errorf("描述内容不对: %q", got)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("请求路径不对: %s", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization 不对: %q", gotAuth)
	}
	if gotModel != "vision-test" {
		t.Errorf("model 不对: %q", gotModel)
	}
	if !strings.HasPrefix(gotDataURL, "data:image/png;base64,") {
		t.Errorf("图片应以 data URL 传递，实际 %q", gotDataURL)
	}
}

func TestVisionDescribeContentParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"第一段"},{"type":"text","text":"第二段"}]}}]}`))
	}))
	defer srv.Close()

	v := &Vision{BaseURL: srv.URL, Model: "m"}
	got, err := v.Describe(context.Background(), []byte("x"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if got != "第一段第二段" {
		t.Errorf("分段内容应拼接，实际 %q", got)
	}
}

func TestVisionDescribeFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream unavailable"}}`))
	}))
	defer srv.Close()

	v := &Vision{BaseURL: srv.URL, Model: "m"}
	_, err := v.Describe(context.Background(), []byte("x"), "image/png")
	if err == nil || !strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("应返回接口错误，实际 %v", err)
	}
}

func TestVisionDescribeEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"   "}}]}`))
	}))
	defer srv.Close()

	v := &Vision{BaseURL: srv.URL, Model: "m"}
	if _, err := v.Describe(context.Background(), []byte("x"), "image/png"); err == nil {
		t.Fatal("空描述应报错")
	}
}

func TestVisionDescribeTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"太慢"}}]}`))
	}))
	defer srv.Close()

	v := &Vision{BaseURL: srv.URL, Model: "m", Timeout: 50 * time.Millisecond}
	if _, err := v.Describe(context.Background(), []byte("x"), "image/png"); err == nil {
		t.Fatal("超时应报错")
	}
}
