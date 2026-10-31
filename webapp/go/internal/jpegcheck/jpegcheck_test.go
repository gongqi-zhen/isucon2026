package jpegcheck

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// corpus は、Go の image/testdata の JPEG（ベースライン、プログレッシブ、各種の間引き、CMYK、RGB、グレー、RST、途中で切れたもの）と、
// image/jpeg で作った大きさの違う画像。
func corpus(t testing.TB) [][]byte {
	t.Helper()
	var files [][]byte
	paths, _ := filepath.Glob(filepath.Join(runtime.GOROOT(), "src", "image", "testdata", "*.jpeg"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, data)
	}
	if len(paths) == 0 {
		t.Log("GOROOT の image/testdata が見つからないので、作った画像だけで試す")
	}
	for _, size := range [][2]int{{1, 1}, {8, 8}, {17, 9}, {16, 33}, {100, 75}} {
		for _, gray := range []bool{false, true} {
			var img image.Image
			if gray {
				m := image.NewGray(image.Rect(0, 0, size[0], size[1]))
				for i := range m.Pix {
					m.Pix[i] = uint8(i * 7)
				}
				img = m
			} else {
				m := image.NewRGBA(image.Rect(0, 0, size[0], size[1]))
				for y := range size[1] {
					for x := range size[0] {
						m.Set(x, y, color.RGBA{uint8(x * 13), uint8(y * 29), uint8(x ^ y), 255})
					}
				}
				img = m
			}
			for _, quality := range []int{1, 50, 100} {
				var buf bytes.Buffer
				if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
					t.Fatal(err)
				}
				files = append(files, buf.Bytes())
			}
		}
	}
	// SOF の高さか幅を 0 にしたもの（jpeg.Decode は空の Bounds で成功することがある）。
	for _, data := range files[:] {
		for i := 0; i+9 < len(data); i++ {
			if data[i] == 0xff && (data[i+1] == 0xc0 || data[i+1] == 0xc2) {
				for _, offset := range []int{5, 7} {
					patched := bytes.Clone(data)
					patched[i+offset], patched[i+offset+1] = 0, 0
					files = append(files, patched)
				}
				break
			}
		}
	}
	return files
}

// compare は、Check と jpeg.Decode の結果（成否、エラーの文、大きさ）が同じことを確かめる。
func compare(t testing.TB, data []byte) {
	t.Helper()
	img, wantErr := jpeg.Decode(bytes.NewReader(data))
	width, height, err := Check(bytes.NewReader(data))
	if (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) {
		t.Fatalf("Check error = %v, jpeg.Decode error = %v (len %d)", err, wantErr, len(data))
	}
	if err == nil && (width != img.Bounds().Dx() || height != img.Bounds().Dy()) {
		t.Fatalf("Check size = %dx%d, jpeg.Decode bounds = %v", width, height, img.Bounds())
	}
}

func TestCheckMatchesDecode(t *testing.T) {
	files := corpus(t)
	for _, data := range files {
		compare(t, data)
	}
}

// 壊した入力でも同じ結果になること。1バイトの書き換え、0xff の挿入、途中での切断、範囲の削除と複製を試す。
func TestCheckMatchesDecodeOnCorruptedInput(t *testing.T) {
	files := corpus(t)
	rng := rand.New(rand.NewPCG(127, 1))
	iterations := 300
	if testing.Short() {
		iterations = 30
	}
	outcomes := map[string]int{}
	for _, original := range files {
		for range iterations {
			data := bytes.Clone(original)
			switch rng.IntN(6) {
			case 0:
				data[rng.IntN(len(data))] = byte(rng.IntN(256))
			case 1:
				data[rng.IntN(len(data))] ^= 1 << rng.IntN(8)
			case 2:
				i := rng.IntN(len(data))
				data = append(data[:i:i], append([]byte{0xff, byte(rng.IntN(256))}, data[i:]...)...)
			case 3:
				data = data[:rng.IntN(len(data))]
			case 4:
				i := rng.IntN(len(data))
				j := min(len(data), i+1+rng.IntN(64))
				data = append(data[:i:i], data[j:]...)
			case 5:
				i := rng.IntN(len(data))
				j := min(len(data), i+1+rng.IntN(64))
				data = append(data[:j:j], append(bytes.Clone(data[i:j]), data[j:]...)...)
			}
			compare(t, data)
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				outcomes[strings.SplitN(err.Error(), ":", 2)[0]]++
			} else {
				outcomes["ok"]++
			}
		}
	}
	t.Logf("files=%d outcomes=%v", len(files), outcomes)
}

func FuzzCheckMatchesDecode(f *testing.F) {
	for _, data := range corpus(f) {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		compare(t, data)
	})
}

func BenchmarkCheck(b *testing.B) {
	m := image.NewRGBA(image.Rect(0, 0, 1024, 768))
	for i := range m.Pix {
		m.Pix[i] = uint8(i * 31 >> 3)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: 85}); err != nil {
		b.Fatal(err)
	}
	data := buf.Bytes()
	b.Run("jpeg.Decode", func(b *testing.B) {
		for b.Loop() {
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Check", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := Check(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
