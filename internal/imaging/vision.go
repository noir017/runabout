package imaging

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const visionPrompt = `请仔细查看这张图片，按顺序完成三件事：
1. 逐字转写图中所有可见文字，保留标题、列表、代码、表格等原有结构；看不清的字用 [?] 标出，不要猜。
2. 描述界面或画面的当前状态：所在页面、窗口、按钮、选中项、进度、数据变化。
3. 如果有报错、警告或异常，原样引用错误信息，并说明它出现在哪里。
不要评价图片质量，不要省略文字。`

// Vision 调用 OpenAI 兼容的 /chat/completions 接口描述图片。
type Vision struct {
	BaseURL string
	Model   string
	APIKey  string
	Timeout time.Duration
	Client  *http.Client
}

// Describe 返回图片的文字描述。调用方负责决定失败时如何降级。
func (v *Vision) Describe(ctx context.Context, data []byte, mime string) (string, error) {
	if strings.TrimSpace(v.BaseURL) == "" || strings.TrimSpace(v.Model) == "" {
		return "", fmt.Errorf("视觉描述未配置")
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload := map[string]any{
		"model": v.Model,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": visionPrompt},
				{"type": "image_url", "image_url": map[string]string{
					"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
				}},
			},
		}},
		"max_tokens": 2000,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	endpoint := strings.TrimRight(v.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("构造视觉请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if v.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+v.APIKey)
	}

	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用视觉接口失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("读取视觉接口响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("视觉接口返回 HTTP %d: %s", resp.StatusCode, apiErrorText(raw))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析视觉接口响应失败: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", fmt.Errorf("视觉接口报错: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("视觉接口没有返回结果")
	}
	text := strings.TrimSpace(contentText(out.Choices[0].Message.Content))
	if text == "" {
		return "", fmt.Errorf("视觉接口返回了空描述")
	}
	return text, nil
}

// contentText 兼容 content 为字符串或分段数组两种返回形式。
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	return ""
}

func apiErrorText(raw []byte) string {
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		return "(空响应)"
	}
	return s
}
