package isureal

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"testing"
)

// legacyJPEGWithin は変更前のハンドラと同じ判定（全体をデコードしてから Bounds を確認する）。
func legacyJPEGWithin(data []byte, maxSide int) bool {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return false
	}
	return img.Bounds().Dx() <= maxSide && img.Bounds().Dy() <= maxSide
}

func encodeTestJPEG(t testing.TB, w, h int, gray bool) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(w*7919 + h)))
	var img image.Image
	if gray {
		g := image.NewGray(image.Rect(0, 0, w, h))
		for i := range g.Pix {
			g.Pix[i] = uint8(rng.Intn(256))
		}
		img = g
	} else {
		rgba := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				rgba.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), uint8(rng.Intn(64)), 255})
			}
		}
		img = rgba
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withJFIF は SOI の直後に JFIF の APP0 セグメントを差し込む。
// image/jpeg の DecodeConfig は JFIF のときだけ SOF を読んだ時点で返るので、その経路も確かめる。
func withJFIF(data []byte) []byte {
	app0 := []byte{0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00}
	out := append([]byte{}, data[:2]...)
	out = append(out, app0...)
	return append(out, data[2:]...)
}

// markerOffset は最初に現れる 0xFF marker の位置を返す。
func markerOffset(t *testing.T, data []byte, marker byte) int {
	t.Helper()
	i := bytes.Index(data, []byte{0xff, marker})
	if i < 0 {
		t.Fatalf("marker %#x not found", marker)
	}
	return i
}

// TestJPEGDecodeVsDecodeConfig は Decode と DecodeConfig の結果が分かれる入力の具体例。
// DecodeConfig だけで検証すると、これらの「壊れた JPEG」を受け付けるようになる。
func TestJPEGDecodeVsDecodeConfig(t *testing.T) {
	base := encodeTestJPEG(t, 64, 48, false)
	jfif := withJFIF(base)
	sos := markerOffset(t, base, 0xda)
	jfifSOS := markerOffset(t, jfif, 0xda)
	jfifDHT := markerOffset(t, jfif, 0xc4)
	// SOF の高さを 0、幅を 5000 にする。Decode は空の画像として成功するので、
	// 変更前のハンドラは受け付けていた。
	zeroHeight := append([]byte{}, base...)
	sof := markerOffset(t, zeroHeight, 0xc0)
	zeroHeight[sof+5], zeroHeight[sof+6] = 0, 0       // height
	zeroHeight[sof+7], zeroHeight[sof+8] = 0x13, 0x88 // width = 5000

	cases := []struct {
		name      string
		data      []byte
		configOK  bool
		decodeOK  bool
		wantValid bool // validJPEGWithin(data, 512) / 変更前ハンドラの判定
	}{
		{"valid", base, true, true, true},
		{"valid JFIF", jfif, true, true, true},
		{"empty", nil, false, false, false},
		{"not JPEG", []byte("this is not a jpeg"), false, false, false},
		{"SOI only", base[:2], false, false, false},
		{"truncated inside scan data", base[:sos+(len(base)-sos)/2], true, false, false},
		{"missing EOI", base[:len(base)-2], true, false, false},
		{"truncated right after SOS header", base[:sos+14], true, false, false},
		{"JFIF truncated before DHT (SOF only)", jfif[:jfifDHT], true, false, false},
		{"JFIF truncated before SOS", jfif[:jfifSOS], true, false, false},
		{"non-JFIF truncated before SOS", base[:sos], false, false, false},
		{"SOF 5000x0 (Decode returns empty bounds)", zeroHeight, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cfgErr := jpeg.DecodeConfig(bytes.NewReader(tc.data))
			_, decErr := jpeg.Decode(bytes.NewReader(tc.data))
			if (cfgErr == nil) != tc.configOK {
				t.Errorf("DecodeConfig ok=%v, want %v (err=%v)", cfgErr == nil, tc.configOK, cfgErr)
			}
			if (decErr == nil) != tc.decodeOK {
				t.Errorf("Decode ok=%v, want %v (err=%v)", decErr == nil, tc.decodeOK, decErr)
			}
			if got := validJPEGWithin(tc.data, 512); got != tc.wantValid {
				t.Errorf("validJPEGWithin=%v, want %v", got, tc.wantValid)
			}
			if got := legacyJPEGWithin(tc.data, 512); got != tc.wantValid {
				t.Errorf("legacy=%v, want %v", got, tc.wantValid)
			}
		})
	}
}

// TestValidJPEGWithinMatchesLegacy は、正常な画像とそれを壊した多数の入力で、
// 新旧の判定が一致し、前提にしている不変条件が成り立つことを確かめる。
//   - DecodeConfig が失敗するなら Decode も失敗する
//   - Decode が成功し、Width と Height が正なら、Width/Height == Bounds().Dx()/Dy()
func TestValidJPEGWithinMatchesLegacy(t *testing.T) {
	type sample struct {
		data    []byte
		maxSide int
	}
	var seeds []sample
	for _, s := range []struct {
		w, h, max int
		gray      bool
	}{
		{512, 512, 512, false}, {513, 16, 512, false}, {16, 513, 512, true},
		{511, 300, 512, true}, {33, 17, 512, false},
		{4096, 8, 4096, false}, {4097, 8, 4096, true}, {8, 4097, 4096, false},
	} {
		img := encodeTestJPEG(t, s.w, s.h, s.gray)
		seeds = append(seeds, sample{img, s.max}, sample{withJFIF(img), s.max})
	}

	check := func(t *testing.T, data []byte, maxSide int, label string) {
		t.Helper()
		cfg, cfgErr := jpeg.DecodeConfig(bytes.NewReader(data))
		img, decErr := jpeg.Decode(bytes.NewReader(data))
		if cfgErr != nil && decErr == nil {
			t.Fatalf("%s: DecodeConfig failed (%v) but Decode succeeded", label, cfgErr)
		}
		if decErr == nil && cfg.Width > 0 && cfg.Height > 0 && (cfg.Width != img.Bounds().Dx() || cfg.Height != img.Bounds().Dy()) {
			t.Fatalf("%s: config %dx%d != bounds %v", label, cfg.Width, cfg.Height, img.Bounds())
		}
		if got, want := validJPEGWithin(data, maxSide), legacyJPEGWithin(data, maxSide); got != want {
			t.Fatalf("%s: validJPEGWithin=%v legacy=%v", label, got, want)
		}
	}

	rng := rand.New(rand.NewSource(13))
	for _, s := range seeds {
		check(t, s.data, s.maxSide, "seed")
		// ヘッダ部分（SOS の直後まで）はすべての長さで切り詰める。それ以降は間引く。
		sos := bytes.Index(s.data, []byte{0xff, 0xda})
		stride := len(s.data)/50 + 1
		for n := 0; n < len(s.data); n++ {
			if n > sos+64 && n%stride != 0 {
				continue
			}
			check(t, s.data[:n], s.maxSide, "truncate")
		}
		// ヘッダを書き換える（サイズ・セグメント長・マーカーの破損）。
		// SOF セグメントと先頭は全バイト、量子化表・ハフマン表の中身は間引く。
		sof := bytes.Index(s.data, []byte{0xff, 0xc0})
		for i := 0; i < sos+14 && i < len(s.data); i++ {
			if !(i < 32 || (sof <= i && i < sof+19) || i >= sos || i%13 == 0) {
				continue
			}
			for _, v := range []byte{0x00, 0x01, 0x7f, 0xff, s.data[i] ^ 0x10} {
				mutated := append([]byte{}, s.data...)
				mutated[i] = v
				check(t, mutated, s.maxSide, "header mutation")
			}
		}
		// 全体のランダムな破損。
		for k := 0; k < 50; k++ {
			mutated := append([]byte{}, s.data...)
			for m := 0; m <= rng.Intn(4); m++ {
				mutated[rng.Intn(len(mutated))] = byte(rng.Intn(256))
			}
			check(t, mutated, s.maxSide, "random mutation")
		}
	}
}

func benchJPEG(b *testing.B, w, h, maxSide int) {
	data := encodeTestJPEG(b, w, h, false)
	b.Run("Decode", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for b.Loop() {
			legacyJPEGWithin(data, maxSide)
		}
	})
	b.Run("DecodeConfig", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for b.Loop() {
			_, _ = jpeg.DecodeConfig(bytes.NewReader(data))
		}
	})
	b.Run("validJPEGWithin", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for b.Loop() {
			validJPEGWithin(data, maxSide)
		}
	})
}

func BenchmarkJPEGPost1080x1440(b *testing.B)    { benchJPEG(b, 1080, 1440, 4096) }
func BenchmarkJPEGRealMoji512(b *testing.B)      { benchJPEG(b, 512, 512, 512) }
func BenchmarkJPEGPostOversize5000(b *testing.B) { benchJPEG(b, 5000, 5000, 4096) }
