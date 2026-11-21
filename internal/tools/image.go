package tools

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/imaging"
	"github.com/noir017/runabout/internal/mcp"
	"github.com/noir017/runabout/internal/media"
)

// imageURLHint 附在普通读图结果末尾（仅服务端启用 image_url 时），
// 让看不到图片的 agent 立刻知道怎么切到链接模式。
const imageURLHint = "提示：如当前客户端看不到图片画面，可带 image_url=true 重读以获取临时下载链接。"

// readImage 把图片按配置缩放/重编码后返回，并附上尺寸变化说明。
// prefix 是 [host] 前缀，target 是结构化输出里的机器名（单机模式为空）。
// asURL 为 true 时不返回 image 块，改为把图片放进临时仓库并返回下载链接。
func (t *readTool) readImage(cc *mcp.CallContext, prefix, abs, mime string, data []byte, target string, asURL bool) *mcp.CallToolResult {
	fs := t.d.Cfg.Tools.FS
	var res *imaging.Result
	var err error
	withImageSlot(t.sem, func() {
		res, err = imaging.Process(data, imaging.Options{
			MaxEdge:   fs.ImageMaxEdge,
			MaxBytes:  fs.MaxImageBytes,
			MaxPixels: fs.MaxImagePixels,
		})
	})
	if err != nil {
		return mcp.ToolError("%s处理图片 %s 失败: %v", prefix, abs, err)
	}

	var header string
	if res.Resized {
		header = fmt.Sprintf("%s图片 %s（原图 %dx%d，%s；已压缩为 %dx%d JPEG，%s，质量 %d）",
			prefix, abs, res.SourceWidth, res.SourceHeight, humanBytes(int64(res.SourceBytes)),
			res.Width, res.Height, humanBytes(int64(len(res.Data))), res.Quality)
	} else {
		header = fmt.Sprintf("%s图片 %s（%s，%dx%d，%s）",
			prefix, abs, mime, res.Width, res.Height, humanBytes(int64(len(res.Data))))
	}

	meta := map[string]any{
		"path": abs, "mime": res.MIME,
		"width": res.Width, "height": res.Height,
		"source_width": res.SourceWidth, "source_height": res.SourceHeight,
		"source_bytes": res.SourceBytes, "bytes": len(res.Data),
		"resized": res.Resized, "quality": res.Quality,
	}
	if target != "" {
		meta["target"] = target
	}

	var content []mcp.Content
	if asURL {
		token, expires, err := t.d.Media.Put(res.Data, res.MIME)
		if err != nil {
			return mcp.ToolError("%s图片 %s 已处理，但生成临时下载链接失败: %v", prefix, abs, err)
		}
		link := t.d.Cfg.Server.BaseURL + media.PathPrefix + token
		meta["url"] = link
		meta["url_expires_at"] = expires.UTC().Format(time.RFC3339)
		content = []mcp.Content{mcp.TextContent(header + "\n\n" +
			imageURLGuide(link, expires, res.MIME, t.d.Media.TTL(), t.d.Media.OneTime()))}
	} else {
		content = []mcp.Content{
			mcp.TextContent(header),
			mcp.ImageContent(base64.StdEncoding.EncodeToString(res.Data), res.MIME),
		}
	}
	// 视觉描述是兜底：失败只附一句原因，图片本身照常返回。
	if vc := fs.Vision; strings.TrimSpace(vc.BaseURL) != "" && strings.TrimSpace(vc.Model) != "" {
		desc, err := describeImage(cc, vc, res.Data, res.MIME)
		if err != nil {
			content = append(content, mcp.TextContent("视觉描述失败："+err.Error()))
		} else {
			content = append(content, mcp.TextContent("视觉描述：\n"+desc))
		}
	}
	if !asURL && t.d.Media != nil {
		content = append(content, mcp.TextContent(imageURLHint))
	}
	return (&mcp.CallToolResult{Content: content}).WithStructured(meta)
}

// imageURLGuide 写给拿到链接的 agent：完整链接、有效期，以及下载后怎么看、看完怎么收尾。
func imageURLGuide(link string, expires time.Time, mime string, ttl time.Duration, oneTime bool) string {
	once := ""
	if oneTime {
		once = "，只能下载一次"
	}
	return fmt.Sprintf(`临时下载链接（%s内有效，%s 过期%s）：
%s

查看方式：用 HTTP GET 把链接下载到本地临时文件，再用本地的文件/图片查看工具打开，例如
  curl -fsSL -o /tmp/runabout-image%s '%s'
看完删除该临时文件。链接过期或失效后，重新调用 read 并带 image_url=true 获取新链接。`,
		humanTTL(ttl), expires.UTC().Format(time.RFC3339), once, link, imageExt(mime), link)
}

func imageExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	}
	return ""
}

// humanTTL 把整分钟/整秒的时长写成中文，其余退回 Duration 的默认格式。
func humanTTL(d time.Duration) string {
	switch {
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d 分钟", int64(d/time.Minute))
	case d >= time.Second && d%time.Second == 0:
		return fmt.Sprintf("%d 秒", int64(d/time.Second))
	}
	return d.String()
}

// linkKind 按 base_url 的 scheme 说明链接类型，避免本机调试时把 http 链接说成 HTTPS。
func linkKind(baseURL string) string {
	if strings.HasPrefix(strings.ToLower(baseURL), "https://") {
		return "HTTPS"
	}
	return "HTTP"
}

// sniffImageMime 读文件头判断是不是支持的图片。不能只看扩展名：
// 改名成 .png 的大文本文件会被误当成图片，绕过 max_read_bytes 整个读进内存。
func sniffImageMime(head []byte) (string, bool) {
	if len(head) == 0 {
		return "", false
	}
	ct := http.DetectContentType(head)
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return ct, true
	}
	// DetectContentType 对 BMP/WebP 的覆盖随版本可能不同，补一手签名。
	if len(head) >= 2 && head[0] == 'B' && head[1] == 'M' {
		return "image/bmp", true
	}
	if len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		return "image/webp", true
	}
	return "", false
}

// describeImage 只从 api_key_env 指定的环境变量取 key，配置里不落密钥。
func describeImage(cc *mcp.CallContext, vc config.VisionCfg, data []byte, mime string) (string, error) {
	key := ""
	if name := strings.TrimSpace(vc.APIKeyEnv); name != "" {
		key = os.Getenv(name)
		if key == "" {
			return "", fmt.Errorf("环境变量 %s 未设置", name)
		}
	}
	v := &imaging.Vision{BaseURL: vc.BaseURL, Model: vc.Model, APIKey: key, Timeout: vc.Timeout}
	return v.Describe(cc.Ctx, data, mime)
}

// withImageSlot 限制同时处理的图片张数；sem 为 nil 时不限制（测试里直接构造 readTool）。
func withImageSlot(sem chan struct{}, fn func()) {
	if sem == nil {
		fn()
		return
	}
	sem <- struct{}{}
	defer func() { <-sem }()
	fn()
}

func imageConcurrency(cfg *config.Config) int {
	if n := cfg.Tools.FS.MaxImageConcurrency; n > 0 {
		return n
	}
	return 2
}
