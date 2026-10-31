package isureal

import (
	"bytes"
	"crypto/sha256"
	"image/jpeg"
	"time"
)

// validJPEGWithin は jpeg.Decode が成功し、縦横とも maxSide 以下なら true を返す。
// 以前の「jpeg.Decode してから Bounds を確認する」処理と同じ判定になるように、
// 先に jpeg.DecodeConfig でサイズを確認し、超過していれば全体のデコードを省く。
//
//   - DecodeConfig が失敗する入力は Decode も必ず失敗する（同じパーサで
//     SOF/SOS までを読むだけなので）。どちらも false。
//   - Width と Height がどちらも正なら、DecodeConfig の Width/Height は
//     Decode 後の Bounds の Dx/Dy と同じ。超過していれば、Decode の成否に
//     かかわらず以前も false だった。
//   - SOF の高さか幅が 0 だと、Decode は空の Bounds（0x0）で成功してしまい、
//     もう一方が上限を超えていても以前は true だった。この場合は省略せずに
//     Decode する。
//   - サイズ内なら今まで通り Decode で全体を検証する。
//
// 失敗の理由（デコード失敗かサイズ超過か）は区別できなくなるので、
// 両者で応答が同じ呼び出し元でだけ使うこと。
func validJPEGWithin(data []byte, maxSide int) bool {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return false
	}
	if cfg.Width > 0 && cfg.Height > 0 && (cfg.Width > maxSide || cfg.Height > maxSide) {
		return false
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return false
	}
	return img.Bounds().Dx() <= maxSide && img.Bounds().Dy() <= maxSide
}

// JPEG の検証結果のキャッシュ（#102）。
//
// 検証の結果は、画像のバイト列と検証の種類だけで決まる（validJPEGWithin も profileImageCheck も、ほかの状態を見ない）。
// ベンチマーカーが同じ画像を何度も送ってくるなら、2回目からは全体のデコード（投稿の画像で 1枚 数 ms）を省ける。
// 判定の方法は変えない（#29、#30）。結果を覚えるだけなので、応答と abuse のログは今と同じになる。
//
// キーはバイト列全体の SHA-256。長さや先頭だけをキーにすると、壊れた JPEG を通った画像と取り違えるおそれがある。
// 初期化（resetImageCaches）では消さない。結果はバイト列だけで決まり、DB の状態に依存しないため。
const jpegVerdictCacheEntries = 100_000

// jpegCheckKind は検証の種類。validJPEGWithin の maxSide（投稿 4096、RealMoji 512）か、profileImageKind。
type jpegCheckKind int

// profileImageKind はプロフィール画像の検証（profileImageCheck）。validJPEGWithin の maxSide とは重ならない。
const profileImageKind jpegCheckKind = -1

type jpegVerdictKey struct {
	sum  [sha256.Size]byte
	kind jpegCheckKind
}

// jpegVerdict は検証の結果。
type jpegVerdict int

const (
	// 通った。
	jpegValid jpegVerdict = iota
	// validJPEGWithin が false を返した（デコードの失敗とサイズの超過を区別しない）。
	jpegInvalid
	// プロフィール画像で、jpeg.Decode が失敗した。abuse（invalid_image）としてログに出す。
	jpegDecodeFailed
	// プロフィール画像で、デコードはできたが、縦か横が 512 を超えた。
	jpegOversized
)

var jpegVerdictCache = newLRUCache[jpegVerdictKey, jpegVerdict](jpegVerdictCacheEntries)

// cachedJPEGVerdict は、覚えている結果があればそれを返し、なければ check を呼んで覚える。
func cachedJPEGVerdict(data []byte, kind jpegCheckKind, check func([]byte) jpegVerdict) jpegVerdict {
	key := jpegVerdictKey{sum: sha256.Sum256(data), kind: kind}
	if verdict, ok := jpegVerdictCache.Get(key); ok {
		return verdict
	}
	verdict := check(data)
	jpegVerdictCache.Add(key, verdict, 1, time.Time{})
	return verdict
}

// cachedValidJPEGWithin は validJPEGWithin と同じ結果を返す。同じバイト列と maxSide の結果を覚えておく。
func cachedValidJPEGWithin(data []byte, maxSide int) bool {
	return cachedJPEGVerdict(data, jpegCheckKind(maxSide), func(data []byte) jpegVerdict {
		if validJPEGWithin(data, maxSide) {
			return jpegValid
		}
		return jpegInvalid
	}) == jpegValid
}

// profileImageCheck は、プロフィール画像の変更前の判定（全体をデコードしてから、縦横が 512 以下かを確かめる）。
// デコードに失敗したら、サイズを見ずに jpegDecodeFailed にする（#30：サイズを先に見ると abuse のログが変わる）。
func profileImageCheck(data []byte) jpegVerdict {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return jpegDecodeFailed
	}
	if img.Bounds().Dx() > 512 || img.Bounds().Dy() > 512 {
		return jpegOversized
	}
	return jpegValid
}

// cachedProfileImageCheck は profileImageCheck と同じ結果を返す。同じバイト列の結果を覚えておく。
func cachedProfileImageCheck(data []byte) jpegVerdict {
	return cachedJPEGVerdict(data, profileImageKind, profileImageCheck)
}
