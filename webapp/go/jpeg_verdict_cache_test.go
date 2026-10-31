package isureal

import (
	"bytes"
	"encoding/json"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

// legacyProfileImageCheck は変更前の putProfileImageHandler と同じ判定。
func legacyProfileImageCheck(data []byte) jpegVerdict {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return jpegDecodeFailed
	}
	if img.Bounds().Dx() > 512 || img.Bounds().Dy() > 512 {
		return jpegOversized
	}
	return jpegValid
}

// jpegVerdictSamples は、TestJPEGDecodeVsDecodeConfig の壊れた JPEG と、サイズの境目の画像。
func jpegVerdictSamples(t *testing.T) map[string][]byte {
	t.Helper()
	base := encodeTestJPEG(t, 64, 48, false)
	jfif := withJFIF(base)
	sos := markerOffset(t, base, 0xda)
	jfifSOS := markerOffset(t, jfif, 0xda)
	jfifDHT := markerOffset(t, jfif, 0xc4)
	zeroHeight := append([]byte{}, base...)
	sof := markerOffset(t, zeroHeight, 0xc0)
	zeroHeight[sof+5], zeroHeight[sof+6] = 0, 0
	zeroHeight[sof+7], zeroHeight[sof+8] = 0x13, 0x88
	return map[string][]byte{
		"valid":                                 base,
		"valid JFIF":                            jfif,
		"empty":                                 nil,
		"not JPEG":                              []byte("this is not a jpeg"),
		"SOI only":                              base[:2],
		"truncated inside scan data":            base[:sos+(len(base)-sos)/2],
		"missing EOI":                           base[:len(base)-2],
		"truncated right after SOS header":      base[:sos+14],
		"JFIF truncated before DHT (SOF only)":  jfif[:jfifDHT],
		"JFIF truncated before SOS":             jfif[:jfifSOS],
		"non-JFIF truncated before SOS":         base[:sos],
		"SOF 5000x0":                            zeroHeight,
		"512x512":                               encodeTestJPEG(t, 512, 512, false),
		"513x16":                                encodeTestJPEG(t, 513, 16, false),
		"16x513 gray":                           encodeTestJPEG(t, 16, 513, true),
		"4096x8":                                encodeTestJPEG(t, 4096, 8, false),
		"4097x8":                                encodeTestJPEG(t, 4097, 8, true),
		"513x16 truncated inside scan data":     truncateScan(t, encodeTestJPEG(t, 513, 16, false)),
		"4097x8 missing EOI":                    dropEOI(encodeTestJPEG(t, 4097, 8, false)),
		"512x512 JFIF truncated before SOS":     jfifBeforeSOS(t, encodeTestJPEG(t, 512, 512, true)),
		"SOF 5000x0 truncated inside scan data": truncateScan(t, zeroHeight),
	}
}

func truncateScan(t *testing.T, data []byte) []byte {
	sos := markerOffset(t, data, 0xda)
	return data[:sos+(len(data)-sos)/2]
}

func dropEOI(data []byte) []byte { return data[:len(data)-2] }

func jfifBeforeSOS(t *testing.T, data []byte) []byte {
	jfif := withJFIF(data)
	return jfif[:markerOffset(t, jfif, 0xda)]
}

// TestCachedJPEGVerdictMatchesUncached は、キャッシュに外れたとき（1回目）も当たったとき（2回目）も、
// 覚えない場合と同じ結果になることを確かめる。
func TestCachedJPEGVerdictMatchesUncached(t *testing.T) {
	jpegVerdictCache.Reset()
	defer jpegVerdictCache.Reset()
	for name, data := range jpegVerdictSamples(t) {
		t.Run(name, func(t *testing.T) {
			for _, maxSide := range []int{512, 4096} {
				want := validJPEGWithin(data, maxSide)
				if legacy := legacyJPEGWithin(data, maxSide); legacy != want {
					t.Fatalf("maxSide=%d: validJPEGWithin=%v legacy=%v", maxSide, want, legacy)
				}
				for attempt := range 2 {
					if got := cachedValidJPEGWithin(data, maxSide); got != want {
						t.Errorf("maxSide=%d attempt=%d: cachedValidJPEGWithin=%v, want %v", maxSide, attempt, got, want)
					}
				}
			}
			want := legacyProfileImageCheck(data)
			if got := profileImageCheck(data); got != want {
				t.Fatalf("profileImageCheck=%v, legacy=%v", got, want)
			}
			for attempt := range 2 {
				if got := cachedProfileImageCheck(data); got != want {
					t.Errorf("attempt=%d: cachedProfileImageCheck=%v, want %v", attempt, got, want)
				}
			}
		})
	}
}

// TestCachedJPEGVerdictSkipsSecondCheck は、同じバイト列と種類なら2回目は検証を呼ばず、
// 種類かバイト列が違えば呼ぶことを確かめる。
func TestCachedJPEGVerdictSkipsSecondCheck(t *testing.T) {
	jpegVerdictCache.Reset()
	defer jpegVerdictCache.Reset()
	data := encodeTestJPEG(t, 1000, 10, false)
	calls := 0
	check := func(maxSide int) func([]byte) jpegVerdict {
		return func(data []byte) jpegVerdict {
			calls++
			if validJPEGWithin(data, maxSide) {
				return jpegValid
			}
			return jpegInvalid
		}
	}
	if got := cachedJPEGVerdict(data, 4096, check(4096)); got != jpegValid || calls != 1 {
		t.Fatalf("first: verdict=%v calls=%d", got, calls)
	}
	if got := cachedJPEGVerdict(data, 4096, check(4096)); got != jpegValid || calls != 1 {
		t.Fatalf("second: verdict=%v calls=%d, want the cached verdict without calling check", got, calls)
	}
	// 1000x10 は 4096 なら通るが 512 なら通らない。種類ごとに別のエントリになること。
	if got := cachedJPEGVerdict(data, 512, check(512)); got != jpegInvalid || calls != 2 {
		t.Fatalf("other kind: verdict=%v calls=%d", got, calls)
	}
	if got := cachedProfileImageCheck(data); got != jpegOversized {
		t.Fatalf("profile: verdict=%v, want oversized", got)
	}
	// 1バイトでも違えば別のエントリになる（末尾の EOI を壊すとデコードに失敗する）。
	broken := append([]byte{}, data...)
	broken[len(broken)-1] = 0x00
	if got := cachedJPEGVerdict(broken, 4096, check(4096)); got != jpegInvalid || calls != 3 {
		t.Fatalf("broken: verdict=%v calls=%d", got, calls)
	}
}

// TestProfileImageRejectionsWithCache は、プロフィール画像の不正な画像を2回送ったとき、
// 2回とも変更前と同じ応答と操作ログ（デコードの失敗だけ abuse: invalid_image）になることを確かめる。
// 400 を返す経路は DB とセッションを使わない。
func TestProfileImageRejectionsWithCache(t *testing.T) {
	jpegVerdictCache.Reset()
	defer jpegVerdictCache.Reset()
	var logs bytes.Buffer
	e := echo.New()
	e.Use(ActionLogMiddleware("info", &logs))
	e.HTTPErrorHandler = APIErrorHandler
	e.PUT("/api/me/profile-image", putProfileImageHandler(&HandlerContext{}))

	base := encodeTestJPEG(t, 64, 48, false)
	cases := []struct {
		name      string
		data      []byte
		wantAbuse string
	}{
		{"missing EOI", base[:len(base)-2], "invalid_image"},
		{"not JPEG", []byte("this is not a jpeg"), "invalid_image"},
		{"513x16", encodeTestJPEG(t, 513, 16, false), ""},
		{"16x513 gray", encodeTestJPEG(t, 16, 513, true), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for attempt := range 2 {
				logs.Reset()
				req := httptest.NewRequest(http.MethodPut, "/api/me/profile-image", bytes.NewReader(tc.data))
				req.Header.Set("Content-Type", "image/jpeg")
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest || !bytes.Contains(rec.Body.Bytes(), []byte("invalid_profile_image")) {
					t.Fatalf("attempt=%d: status=%d body=%s", attempt, rec.Code, rec.Body.String())
				}
				var entry map[string]any
				if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
					t.Fatalf("attempt=%d: action log %q: %v", attempt, logs.String(), err)
				}
				abuse, _ := entry["abuse"].(string)
				if abuse != tc.wantAbuse {
					t.Errorf("attempt=%d: abuse=%q, want %q", attempt, abuse, tc.wantAbuse)
				}
			}
		})
	}
}

// BenchmarkJPEGVerdictCacheHit は、キャッシュに当たったときのコスト（バイト列全体の SHA-256 と LRU の参照）。
// 外れたときは、これに validJPEGWithin（BenchmarkJPEGPost1080x1440）の分が足される。
func BenchmarkJPEGVerdictCacheHit(b *testing.B) {
	data := encodeTestJPEG(b, 1080, 1440, false)
	jpegVerdictCache.Reset()
	defer jpegVerdictCache.Reset()
	cachedValidJPEGWithin(data, 4096)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		cachedValidJPEGWithin(data, 4096)
	}
}
