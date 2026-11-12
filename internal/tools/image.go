package tools

import (
	"encoding/base64"
	"fmt"
	"net/http"

	"github.com/noir017/runabout/internal/imaging"
	"github.com/noir017/runabout/internal/mcp"
)

// readImage 把图片按配置缩放/重编码后返回，并附上尺寸变化说明。
// prefix 是 [host] 前缀，target 是结构化输出里的机器名（单机模式为空）。
func (t *readTool) readImage(cc *mcp.CallContext, prefix, abs, mime string, data []byte, target string) *mcp.CallToolResult {
	fs := t.d.Cfg.Tools.FS
	res, err := imaging.Process(data, imaging.Options{
		MaxEdge:  fs.ImageMaxEdge,
		MaxBytes: fs.MaxImageBytes,
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

	content := []mcp.Content{
		mcp.TextContent(header),
		mcp.ImageContent(base64.StdEncoding.EncodeToString(res.Data), res.MIME),
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
	return (&mcp.CallToolResult{Content: content}).WithStructured(meta)
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
