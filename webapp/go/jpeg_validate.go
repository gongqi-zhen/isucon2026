package isureal

import (
	"bytes"
	"image/jpeg"
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
