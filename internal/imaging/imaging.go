// Package imaging 在图片返回给模型前做尺寸与体积控制：
// 解码前限制像素数，超限的图缩放到长边上限并重新编码为 JPEG。
package imaging

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"

	_ "image/gif"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	// DefaultMaxEdge 是长边上限：超过它的图会被缩小。
	DefaultMaxEdge = 2000
	// DefaultMaxBytes 是处理后图片的体积上限。
	DefaultMaxBytes = 4 << 20
	// DefaultMaxPixels 是解码前的像素数上限，用来挡住解压炸弹。
	// 30MP 已覆盖 6K 截图，解码后约 120MB，容器里不至于打爆内存。
	DefaultMaxPixels = 30_000_000

	initialQuality = 85
	minQuality     = 40
	maxRounds      = 16
)

// Options 控制一次处理。零值字段会取默认值。
type Options struct {
	MaxEdge   int
	MaxBytes  int
	MaxPixels int
}

func (o Options) withDefaults() Options {
	if o.MaxEdge <= 0 {
		o.MaxEdge = DefaultMaxEdge
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxPixels <= 0 {
		o.MaxPixels = DefaultMaxPixels
	}
	return o
}

// Result 是一次处理的结果。Resized 为 false 时 Data 与输入完全相同。
type Result struct {
	Data         []byte
	MIME         string
	Width        int
	Height       int
	SourceWidth  int
	SourceHeight int
	SourceBytes  int
	Resized      bool
	Quality      int
}

// Process 按需缩放并编码图片。支持的格式：png、jpeg、gif（取第一帧）、webp、bmp。
func Process(data []byte, opt Options) (*Result, error) {
	opt = opt.withDefaults()

	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("无法解析图片（支持 png/jpeg/gif/webp/bmp）: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("图片尺寸异常: %dx%d", cfg.Width, cfg.Height)
	}
	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > int64(opt.MaxPixels) {
		return nil, fmt.Errorf("图片 %dx%d 共 %d 像素，超过解码上限 %d 像素；请先用 shell 里的图像工具缩小",
			cfg.Width, cfg.Height, pixels, opt.MaxPixels)
	}
	mime := mimeForFormat(format)
	if mime == "" {
		return nil, fmt.Errorf("不支持的图片格式 %q", format)
	}

	res := &Result{
		Data: data, MIME: mime,
		Width: cfg.Width, Height: cfg.Height,
		SourceWidth: cfg.Width, SourceHeight: cfg.Height,
		SourceBytes: len(data),
	}
	if cfg.Width <= opt.MaxEdge && cfg.Height <= opt.MaxEdge && len(data) <= opt.MaxBytes {
		return res, nil
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解码图片失败: %w", err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	tw, th := w, h
	if longEdge := max(w, h); longEdge > opt.MaxEdge {
		scale := float64(opt.MaxEdge) / float64(longEdge)
		tw = max(1, int(math.Round(float64(w)*scale)))
		th = max(1, int(math.Round(float64(h)*scale)))
	}

	quality := initialQuality
	var last bytes.Buffer
	for round := 0; round < maxRounds; round++ {
		last.Reset()
		if err := jpeg.Encode(&last, render(src, tw, th), &jpeg.Options{Quality: quality}); err != nil {
			return nil, fmt.Errorf("编码 JPEG 失败: %w", err)
		}
		if last.Len() <= opt.MaxBytes {
			out := make([]byte, last.Len())
			copy(out, last.Bytes())
			res.Data = out
			res.MIME = "image/jpeg"
			res.Width, res.Height = tw, th
			res.Resized = true
			res.Quality = quality
			return res, nil
		}
		// 先降质量，到下限后继续缩尺寸；按超出比例估缩放系数，
		// 一次缩到位，避免在噪声图上磨很多轮。
		if quality > minQuality {
			quality = max(minQuality, quality-10)
			continue
		}
		if tw <= 64 && th <= 64 {
			break
		}
		ratio := math.Sqrt(float64(opt.MaxBytes) / float64(last.Len()))
		scale := math.Min(0.95, math.Max(0.80, ratio*0.95))
		tw = max(64, int(float64(tw)*scale))
		th = max(64, int(float64(th)*scale))
		quality = 80
	}
	return nil, fmt.Errorf("图片压缩到 %dx%d、质量 %d 后仍有 %d 字节，超过上限 %d 字节",
		tw, th, quality, last.Len(), opt.MaxBytes)
}

// render 把 src 缩放到 w×h 并铺在白底上。JPEG 没有 alpha，
// 透明区域直接编码会变成黑色，截图和图表会很难看。
func render(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

func mimeForFormat(format string) string {
	switch format {
	case "png":
		return "image/png"
	case "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "bmp":
		return "image/bmp"
	}
	return ""
}
