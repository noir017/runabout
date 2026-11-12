package imaging

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"

	"golang.org/x/image/bmp"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gradient(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	return img
}

func TestProcessResizesLargeImage(t *testing.T) {
	src := encodePNG(t, gradient(3000, 1500))
	res, err := Process(src, Options{MaxEdge: 2000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resized {
		t.Fatal("超过长边上限的图应当被缩放")
	}
	if res.Width != 2000 || res.Height != 1000 {
		t.Errorf("缩放尺寸应为 2000x1000，实际 %dx%d", res.Width, res.Height)
	}
	if res.MIME != "image/jpeg" || res.Quality != 85 {
		t.Errorf("应编码为质量 85 的 JPEG，实际 %s q=%d", res.MIME, res.Quality)
	}
	if res.SourceWidth != 3000 || res.SourceHeight != 1500 || res.SourceBytes != len(src) {
		t.Errorf("原图信息不对: %+v", res)
	}
	img, format, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("处理结果不是有效图片: %v", err)
	}
	if format != "jpeg" || img.Bounds().Dx() != 2000 || img.Bounds().Dy() != 1000 {
		t.Errorf("解码结果不对: %s %v", format, img.Bounds())
	}
}

func TestProcessKeepsSmallImage(t *testing.T) {
	src := encodePNG(t, gradient(120, 80))
	res, err := Process(src, Options{MaxEdge: 2000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resized {
		t.Error("未超限的图不应重新编码")
	}
	if !bytes.Equal(res.Data, src) || res.MIME != "image/png" {
		t.Error("未超限时应原样返回")
	}
	if res.Width != 120 || res.Height != 80 {
		t.Errorf("尺寸不对: %dx%d", res.Width, res.Height)
	}
}

func TestProcessShrinksUntilUnderByteLimit(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	img := image.NewRGBA(image.Rect(0, 0, 800, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 800; x++ {
			img.Set(x, y, color.RGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)),
				B: uint8(rng.Intn(256)), A: 255})
		}
	}
	src := encodePNG(t, img)
	const maxBytes = 100 << 10
	if len(src) <= maxBytes {
		t.Fatalf("测试素材本身应超过上限，实际 %d 字节", len(src))
	}
	res, err := Process(src, Options{MaxEdge: 2000, MaxBytes: maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resized {
		t.Fatal("体积超限的图应当被重新编码")
	}
	if len(res.Data) > maxBytes {
		t.Errorf("结果仍有 %d 字节，超过上限 %d", len(res.Data), maxBytes)
	}
	if res.Width > 800 || res.Height > 800 {
		t.Errorf("结果不应被放大: %dx%d", res.Width, res.Height)
	}
}

func TestProcessRejectsPixelBomb(t *testing.T) {
	// 只构造签名和 IHDR：DecodeConfig 能拿到尺寸，但不会真正解码像素。
	src := craftPNG(100_000, 100_000)
	_, err := Process(src, Options{MaxPixels: 1_000_000})
	if err == nil {
		t.Fatal("超大像素数必须在解码前被拒绝")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("像素")) {
		t.Errorf("错误信息应说明像素上限，实际: %v", err)
	}
}

func TestProcessGIFFirstFrame(t *testing.T) {
	pal := color.Palette{color.Black, color.White}
	anim := &gif.GIF{}
	for i := 0; i < 2; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, 40, 20), pal)
		for x := 0; x < 40; x++ {
			frame.SetColorIndex(x, 10, uint8(i%2))
		}
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatal(err)
	}
	res, err := Process(buf.Bytes(), Options{MaxEdge: 20, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resized || res.Width != 20 || res.Height != 10 || res.MIME != "image/jpeg" {
		t.Errorf("GIF 应取第一帧并缩放到 20x10，实际 %+v", res)
	}
}

func TestProcessBMP(t *testing.T) {
	var buf bytes.Buffer
	if err := bmp.Encode(&buf, gradient(100, 50)); err != nil {
		t.Fatal(err)
	}
	res, err := Process(buf.Bytes(), Options{MaxEdge: 60, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resized || res.Width != 60 || res.Height != 30 {
		t.Errorf("BMP 应缩放到 60x30，实际 %dx%d", res.Width, res.Height)
	}
}

func TestProcessWebP(t *testing.T) {
	// 1x1 的静态 WebP 测试向量。
	data, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Process(data, Options{})
	if err != nil {
		t.Fatalf("WebP 应能解码: %v", err)
	}
	if res.MIME != "image/webp" || res.Resized {
		t.Errorf("小 WebP 应原样返回，实际 %+v", res)
	}
}

func TestProcessCorrupt(t *testing.T) {
	if _, err := Process([]byte("这不是图片"), Options{}); err == nil {
		t.Fatal("无法解析的数据应报错")
	}
}

func TestProcessJPEGUnderLimit(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, gradient(64, 64), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	res, err := Process(buf.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.MIME != "image/jpeg" || res.Resized {
		t.Errorf("小 JPEG 应原样返回，实际 %+v", res)
	}
}

func craftPNG(w, h uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8 // 位深
	ihdr[9] = 6 // RGBA
	writeChunk(&buf, "IHDR", ihdr)
	writeChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func writeChunk(buf *bytes.Buffer, typ string, data []byte) {
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
