package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/mcp"
)

func writePNG(t *testing.T, path string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func callRaw(t *testing.T, h mcp.Handler, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Call(&mcp.CallContext{Ctx: context.Background(),
		Principal: mcp.Principal{Subject: "tester"}}, raw)
	if err != nil {
		t.Fatalf("工具返回协议级错误: %v", err)
	}
	return res
}

func findImage(t *testing.T, res *mcp.CallToolResult) (data []byte, mime string) {
	t.Helper()
	for _, c := range res.Content {
		if c.Type == "image" {
			b, err := base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				t.Fatalf("图片内容不是合法 base64: %v", err)
			}
			return b, c.MimeType
		}
	}
	t.Fatal("结果里没有 image 内容块")
	return nil, ""
}

func textOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

func TestReadImageCompresses(t *testing.T) {
	d, dir := newTestDeps(t)
	d.Cfg.Tools.FS.ImageMaxEdge = 200
	d.Cfg.Tools.FS.MaxImageBytes = 1 << 20
	path := filepath.Join(dir, "shot.png")
	writePNG(t, path, 600, 300)

	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("读图失败: %s", textOf(res))
	}
	text := textOf(res)
	if !strings.Contains(text, "600x300") || !strings.Contains(text, "200x100") {
		t.Errorf("文字里应同时给出原尺寸和处理后尺寸:\n%s", text)
	}
	data, mime := findImage(t, res)
	if mime != "image/jpeg" {
		t.Errorf("压缩后应为 JPEG，实际 %s", mime)
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil || format != "jpeg" {
		t.Fatalf("返回的图片无法解码: %v %s", err, format)
	}
	if img.Bounds().Dx() != 200 || img.Bounds().Dy() != 100 {
		t.Errorf("压缩尺寸不对: %v", img.Bounds())
	}
}

func TestReadImageKeepsSmallFile(t *testing.T) {
	d, dir := newTestDeps(t)
	path := filepath.Join(dir, "small.png")
	original := writePNG(t, path, 120, 80)

	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("读图失败: %s", textOf(res))
	}
	data, mime := findImage(t, res)
	if mime != "image/png" || !bytes.Equal(data, original) {
		t.Error("未超限的图片应原样返回")
	}
}

func TestReadImageSourceLimit(t *testing.T) {
	d, dir := newTestDeps(t)
	d.Cfg.Tools.FS.MaxImageSourceBytes = 200
	path := filepath.Join(dir, "big.png")
	writePNG(t, path, 100, 100)

	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": path})
	if !isErr {
		t.Fatal("超过原始大小上限的图片应被拒绝")
	}
	if !strings.Contains(out, "原始大小") {
		t.Errorf("错误信息应说明是原始大小上限:\n%s", out)
	}
}

// 图片判定必须看 magic：扩展名是 .txt 的 PNG 也要按图片处理。
func TestReadImageMagicOverridesExtension(t *testing.T) {
	d, dir := newTestDeps(t)
	d.Cfg.Tools.FS.MaxReadBytes = 50
	d.Cfg.Tools.FS.MaxImageSourceBytes = 1 << 20
	path := filepath.Join(dir, "shot.txt")
	writePNG(t, path, 120, 80)

	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("magic 是 PNG 的文件应按图片读取: %s", textOf(res))
	}
	if _, mime := findImage(t, res); mime != "image/png" {
		t.Errorf("应按 PNG 返回，实际 %s", mime)
	}
}

// 扩展名是 .png 的大文本不能走图片的宽松上限。
func TestReadFakeImageUsesTextLimit(t *testing.T) {
	d, dir := newTestDeps(t)
	d.Cfg.Tools.FS.MaxReadBytes = 50
	d.Cfg.Tools.FS.MaxImageSourceBytes = 1 << 20
	path := filepath.Join(dir, "fake.png")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 200), 0o644); err != nil {
		t.Fatal(err)
	}

	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": path})
	if !isErr {
		t.Fatal("改名成 .png 的文本文件应按文本大小上限拒绝")
	}
	if !strings.Contains(out, "超过单次读取上限") {
		t.Errorf("应命中 max_read_bytes 而不是图片上限:\n%s", out)
	}
}

func TestReadImagePixelLimitFromConfig(t *testing.T) {
	d, dir := newTestDeps(t)
	d.Cfg.Tools.FS.MaxImagePixels = 100
	path := filepath.Join(dir, "small.png")
	writePNG(t, path, 20, 20) // 400 像素

	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": path})
	if !isErr || !strings.Contains(out, "像素") {
		t.Errorf("配置的像素上限应生效:\n%s", out)
	}
}

func TestImageSemaphore(t *testing.T) {
	sem := make(chan struct{}, 1)
	var active, peak int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			withImageSlot(sem, func() {
				cur := atomic.AddInt32(&active, 1)
				for {
					p := atomic.LoadInt32(&peak)
					if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt32(&active, -1)
			})
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Errorf("信号量为 1 时同时处理数应为 1，实际 %d", peak)
	}
}

func TestReadToolSemaphoreWiring(t *testing.T) {
	d, _ := newTestDeps(t)
	d.Cfg.Tools.FS.MaxImageConcurrency = 3
	var rt *readTool
	for _, h := range All(d) {
		if r, ok := h.(*readTool); ok {
			rt = r
		}
	}
	if rt == nil || cap(rt.sem) != 3 {
		t.Errorf("readTool 应按配置初始化信号量，实际 %+v", rt)
	}
}

func TestReadImagePixelBomb(t *testing.T) {
	d, dir := newTestDeps(t)
	path := filepath.Join(dir, "bomb.png")
	// 直接构造 IHDR：DecodeConfig 能读到尺寸，但不会分配像素内存。
	bomb := craftPNGHeader(200_000, 200_000)
	if err := os.WriteFile(path, bomb, 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": path})
	if !isErr || !strings.Contains(out, "像素") {
		t.Errorf("超大像素数应在解码前被拒绝:\n%s", out)
	}
}

func TestReadImageVision(t *testing.T) {
	d, dir := newTestDeps(t)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"界面显示：登录失败，密码错误。"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_VISION_KEY", "secret")
	d.Cfg.Tools.FS.Vision = config.VisionCfg{
		BaseURL: srv.URL, Model: "vision-test", APIKeyEnv: "TEST_VISION_KEY", Timeout: time.Second,
	}
	path := filepath.Join(dir, "ui.png")
	writePNG(t, path, 80, 40)

	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("读图失败: %s", textOf(res))
	}
	text := textOf(res)
	if !strings.Contains(text, "视觉描述") || !strings.Contains(text, "登录失败") {
		t.Errorf("应附上视觉描述:\n%s", text)
	}
	if _, mime := findImage(t, res); mime != "image/png" {
		t.Errorf("图片本身应照常返回，实际 %s", mime)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("key 应从环境变量读取并放进 Bearer 头，实际 %q", gotAuth)
	}
}

func TestReadImageVisionFailureKeepsImage(t *testing.T) {
	d, dir := newTestDeps(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
	}))
	defer srv.Close()
	d.Cfg.Tools.FS.Vision = config.VisionCfg{BaseURL: srv.URL, Model: "m", Timeout: time.Second}
	path := filepath.Join(dir, "ui.png")
	writePNG(t, path, 80, 40)

	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("视觉描述失败不应让读图失败: %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "视觉描述失败") || !strings.Contains(textOf(res), "HTTP 500") {
		t.Errorf("应注明失败原因（状态码），且不回显响应体:\n%s", textOf(res))
	}
	if _, mime := findImage(t, res); mime != "image/png" {
		t.Errorf("图片本身应照常返回，实际 %s", mime)
	}
}

func TestReadImageVisionMissingKey(t *testing.T) {
	d, dir := newTestDeps(t)
	d.Cfg.Tools.FS.Vision = config.VisionCfg{
		BaseURL: "http://127.0.0.1:1", Model: "m", APIKeyEnv: "RB_TEST_NO_SUCH_KEY",
	}
	path := filepath.Join(dir, "ui.png")
	writePNG(t, path, 40, 20)

	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("缺 key 不应让读图失败: %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "RB_TEST_NO_SUCH_KEY") {
		t.Errorf("应说明缺哪个环境变量:\n%s", textOf(res))
	}
}

func craftPNGHeader(w, h uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8
	ihdr[9] = 6
	writePNGChunk(&buf, "IHDR", ihdr)
	writePNGChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func writePNGChunk(buf *bytes.Buffer, typ string, data []byte) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(data)))
	buf.Write(l[:])
	buf.WriteString(typ)
	buf.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc.Sum32())
	buf.Write(c[:])
}
