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
	"github.com/noir017/runabout/internal/media"
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

func TestReadRemoteImageCompressed(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	d.Cfg.Tools.FS.ImageMaxEdge = 200
	path := filepath.Join(dir, "remote-big.png")
	writePNG(t, path, 600, 300)

	res := callRaw(t, &readTool{d: d}, map[string]any{"target": "web1", "path": path})
	if res.IsError {
		t.Fatalf("远端读图失败: %s", textOf(res))
	}
	text := textOf(res)
	if !strings.Contains(text, "[web1]") {
		t.Errorf("图片说明应带机器名: %s", text)
	}
	if !strings.Contains(text, "600x300") || !strings.Contains(text, "200x100") {
		t.Errorf("应给出原尺寸和处理后尺寸: %s", text)
	}
	if _, mime := findImage(t, res); mime != "image/jpeg" {
		t.Errorf("远端大图应压缩成 JPEG，实际 %s", mime)
	}
}

func TestReadRemoteFakeImageUsesTextLimit(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ssh.log")
	d, dir := newFleetDepsEnv(t, config.Fleet{}, map[string]string{"STUB_LOG": logPath})
	d.Cfg.Tools.FS.MaxReadBytes = 50
	d.Cfg.Tools.FS.MaxImageSourceBytes = 1 << 20
	path := filepath.Join(dir, "fake.png")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 200), 0o644); err != nil {
		t.Fatal(err)
	}

	out, isErr := call(t, &readTool{d: d}, map[string]any{"target": "web1", "path": path})
	if !isErr {
		t.Fatal("改名成 .png 的远端文本应按文本上限拒绝")
	}
	if !strings.Contains(out, "超过单次读取上限") {
		t.Errorf("应命中 max_read_bytes 而不是图片上限: %s", out)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "head -c 512") {
		t.Errorf("远端应先读文件头嗅探 magic: %s", log)
	}
	if strings.Contains(string(log), "cat --") {
		t.Errorf("判定为文本前不应把整个文件拉回来: %s", log)
	}
}

// ---------- image_url：按需临时图片链接 ----------

func enableImageURL(t *testing.T, d *Deps, opts media.Options) {
	t.Helper()
	d.Cfg.Server.BaseURL = "https://mcp.example.com"
	d.Media = media.New(opts)
	t.Cleanup(d.Media.Close)
}

func hasImageBlock(res *mcp.CallToolResult) bool {
	for _, c := range res.Content {
		if c.Type == "image" {
			return true
		}
	}
	return false
}

func readSchemaProps(t *testing.T, d *Deps) (string, map[string]any) {
	t.Helper()
	def := (&readTool{d: d}).Definition()
	schema, _ := def.InputSchema.(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	return def.Description, props
}

// 未启用 image_url 时，read 的 schema、说明与读图结果都和以前完全一样。
func TestReadImageURLOffKeepsNativeImage(t *testing.T) {
	d, dir := newTestDeps(t)
	desc, props := readSchemaProps(t, d)
	if _, ok := props["image_url"]; ok {
		t.Error("未启用时 schema 不应出现 image_url")
	}
	if strings.Contains(desc, "image_url") {
		t.Errorf("未启用时说明里不应提 image_url:\n%s", desc)
	}

	path := filepath.Join(dir, "a.png")
	original := writePNG(t, path, 60, 40)
	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("读图失败: %s", textOf(res))
	}
	if data, _ := findImage(t, res); !bytes.Equal(data, original) {
		t.Error("默认应照常返回原生 image 块")
	}
	if strings.Contains(textOf(res), "image_url") {
		t.Errorf("未启用时不应附加 image_url 提示:\n%s", textOf(res))
	}
	meta := res.StructuredContent.(map[string]any)
	if _, ok := meta["url"]; ok {
		t.Error("默认不应生成临时链接")
	}
}

func TestReadImageURLDisabledRejects(t *testing.T) {
	d, dir := newTestDeps(t)
	path := filepath.Join(dir, "a.png")
	writePNG(t, path, 20, 20)
	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": path, "image_url": true})
	if !isErr {
		t.Fatal("未启用时传 image_url=true 应返回错误")
	}
	if !strings.Contains(out, "未启用") || !strings.Contains(out, "tools.fs.image_url") {
		t.Errorf("错误应说明服务端未启用及对应配置项:\n%s", out)
	}
}

// 启用后不传 image_url：仍返回原生 image 块，末尾多一句切换提示。
func TestReadImageURLEnabledDefaultsToNative(t *testing.T) {
	d, dir := newTestDeps(t)
	enableImageURL(t, d, media.Options{})

	desc, props := readSchemaProps(t, d)
	prop, ok := props["image_url"].(map[string]any)
	if !ok || prop["type"] != "boolean" {
		t.Fatalf("启用后 schema 应有布尔参数 image_url，实际 %v", props["image_url"])
	}
	pdesc, _ := prop["description"].(string)
	for _, want := range []string{"默认 false", "无法直接展示 MCP 图片", "5 分钟内有效", "HTTPS", "下载到本地"} {
		if !strings.Contains(pdesc, want) {
			t.Errorf("image_url 参数说明缺少 %q:\n%s", want, pdesc)
		}
	}
	for _, want := range []string{"image_url=true", "无法直接展示 MCP 图片块", "HTTPS 临时下载链接"} {
		if !strings.Contains(desc, want) {
			t.Errorf("工具说明缺少 %q:\n%s", want, desc)
		}
	}
	if strings.Contains(strings.ToLower(desc+pdesc), "gemini") {
		t.Error("说明不应写死特定客户端名")
	}

	path := filepath.Join(dir, "a.png")
	original := writePNG(t, path, 60, 40)
	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("读图失败: %s", textOf(res))
	}
	if data, _ := findImage(t, res); !bytes.Equal(data, original) {
		t.Error("不传 image_url 时应照常返回原生 image 块")
	}
	last := res.Content[len(res.Content)-1]
	if last.Type != "text" || last.Text != imageURLHint {
		t.Errorf("结果末尾应附切换提示，实际 %+v", last)
	}
	if _, ok := res.StructuredContent.(map[string]any)["url"]; ok {
		t.Error("不传 image_url 时不应生成临时链接")
	}
}

func TestReadImageURLReturnsLink(t *testing.T) {
	d, dir := newTestDeps(t)
	enableImageURL(t, d, media.Options{})
	path := filepath.Join(dir, "a.png")
	original := writePNG(t, path, 60, 40)

	before := time.Now()
	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path, "image_url": true})
	if res.IsError {
		t.Fatalf("读图失败: %s", textOf(res))
	}
	if hasImageBlock(res) {
		t.Error("image_url=true 时不应再返回 image 块")
	}
	meta := res.StructuredContent.(map[string]any)
	link, _ := meta["url"].(string)
	const prefix = "https://mcp.example.com/media/"
	if !strings.HasPrefix(link, prefix) {
		t.Fatalf("structuredContent.url 应为 base_url/media/<token>，实际 %q", link)
	}
	exp, err := time.Parse(time.RFC3339, meta["url_expires_at"].(string))
	if err != nil {
		t.Fatalf("url_expires_at 应为 RFC3339: %v", err)
	}
	if d := exp.Sub(before); d < 4*time.Minute || d > 6*time.Minute {
		t.Errorf("过期时间应约为 5 分钟后，实际相差 %v", d)
	}

	text := textOf(res)
	for _, want := range []string{link, "5 分钟内有效", meta["url_expires_at"].(string), "HTTP GET", "curl", ".png", "删除该临时文件", "image_url=true"} {
		if !strings.Contains(text, want) {
			t.Errorf("返回文本缺少 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, imageURLHint) {
		t.Error("链接模式下不应再附切换提示")
	}

	data, mime, ok := d.Media.Get(strings.TrimPrefix(link, prefix))
	if !ok || mime != "image/png" || !bytes.Equal(data, original) {
		t.Errorf("临时仓库里应是处理后的图片: ok=%v mime=%s", ok, mime)
	}
}

// 链接模式存的是压缩后的图片，不是原图。
func TestReadImageURLStoresCompressed(t *testing.T) {
	d, dir := newTestDeps(t)
	enableImageURL(t, d, media.Options{OneTime: true})
	d.Cfg.Tools.FS.ImageMaxEdge = 200
	path := filepath.Join(dir, "big.png")
	writePNG(t, path, 600, 300)

	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path, "image_url": true})
	if res.IsError {
		t.Fatalf("读图失败: %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "只能下载一次") || !strings.Contains(textOf(res), ".jpg") {
		t.Errorf("一次性链接应说明只能下载一次，扩展名应随压缩后的 JPEG:\n%s", textOf(res))
	}
	link := res.StructuredContent.(map[string]any)["url"].(string)
	data, mime, ok := d.Media.Get(link[strings.LastIndex(link, "/")+1:])
	if !ok || mime != "image/jpeg" {
		t.Fatalf("应存压缩后的 JPEG: ok=%v mime=%s", ok, mime)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != 200 {
		t.Errorf("存的图片尺寸不对: %v %v", err, img)
	}
}

func TestReadImageURLWithVision(t *testing.T) {
	d, dir := newTestDeps(t)
	enableImageURL(t, d, media.Options{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"界面显示：保存成功。"}}]}`))
	}))
	defer srv.Close()
	d.Cfg.Tools.FS.Vision = config.VisionCfg{BaseURL: srv.URL, Model: "m", Timeout: time.Second}
	path := filepath.Join(dir, "ui.png")
	writePNG(t, path, 40, 20)

	res := callRaw(t, &readTool{d: d}, map[string]any{"path": path, "image_url": true})
	if res.IsError {
		t.Fatalf("读图失败: %s", textOf(res))
	}
	text := textOf(res)
	if !strings.Contains(text, "视觉描述") || !strings.Contains(text, "保存成功") || !strings.Contains(text, "/media/") {
		t.Errorf("链接模式下视觉描述应照常附加:\n%s", text)
	}
	if hasImageBlock(res) {
		t.Error("链接模式下不应返回 image 块")
	}
}

func TestReadImageURLStoreFull(t *testing.T) {
	d, dir := newTestDeps(t)
	enableImageURL(t, d, media.Options{MaxTotalBytes: 16})
	path := filepath.Join(dir, "a.png")
	writePNG(t, path, 40, 40)
	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": path, "image_url": true})
	if !isErr || !strings.Contains(out, "生成临时下载链接失败") {
		t.Errorf("放不进临时仓库时应明确报错:\n%s", out)
	}
}

func TestReadTextIgnoresImageURL(t *testing.T) {
	d, dir := newTestDeps(t)
	enableImageURL(t, d, media.Options{})
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": path, "image_url": true})
	if isErr {
		t.Fatalf("文本文件带 image_url 不应报错: %s", out)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "只对图片生效") {
		t.Errorf("应照常读文本并说明 image_url 已忽略:\n%s", out)
	}
}

func TestImageURLHelpers(t *testing.T) {
	cases := map[time.Duration]string{
		5 * time.Minute:         "5 分钟",
		90 * time.Second:        "90 秒",
		1500 * time.Millisecond: "1.5s",
	}
	for in, want := range cases {
		if got := humanTTL(in); got != want {
			t.Errorf("humanTTL(%v)=%q，期望 %q", in, got, want)
		}
	}
	if linkKind("https://mcp.example.com") != "HTTPS" || linkKind("http://127.0.0.1:8484") != "HTTP" {
		t.Error("linkKind 应按 scheme 区分")
	}
}
