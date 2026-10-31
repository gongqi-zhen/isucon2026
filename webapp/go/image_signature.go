package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

const imageURLLifetime = 24 * time.Hour
const imageSignatureIterations = 1000

// 署名付きURLの exp をこの単位で切り捨て、同じ期間内は署名を使い回す。
// 切り捨てるので有効期限は imageURLLifetime を超えない。
const imageURLExpGranularity = 60

type signedURLEntry struct {
	exp int64
	sig string
}

func imageSigningKey(secretKeyHash string) ([]byte, error) {
	key, err := hex.DecodeString(secretKeyHash)
	if err != nil || len(key) != sha256.Size {
		return nil, fmt.Errorf("invalid image signing key")
	}
	return key, nil
}

// HMAC を1つだけ作り、Reset で使い回す。Reset は鍵から作った ipad と opad の状態を覚えているので、
// 毎回 hmac.New する場合と比べて、1回ごとの SHA-256 の圧縮が4回から2回に減る。結果は同じ（#71）。
func imageSignature(key []byte, imageID string, exp int64) []byte {
	mac := hmac.New(sha256.New, key)
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	for range imageSignatureIterations {
		mac.Reset()
		mac.Write(value)
		// value はすでに書き込んだので、同じ領域に結果を書いてよい。
		value = mac.Sum(value[:0])
	}
	return value
}

// verifiedSignatureCache のキー。exp と sig は URL のクエリの文字列のまま。
func verifiedSignatureKey(imageID, exp, sig string) string {
	return imageID + "\x00" + exp + "\x00" + sig
}

// user は画像の所有者であること（投稿は作者、RealMoji はリアクションした人、プロフィール画像は本人）。
// 署名に使う所有者の鍵と id をここで覚えておき、画像の取得時に所有者を DB に問い合わせる往復を省く。
// 所有者の id は、画像の有効性の確認で image_objects を読まずに済ませるのに使う。
func signedImageURL(user User, imageID string) string {
	return signedImageURLAt(user, imageID, time.Time{})
}

// signedImageURLAt は signedImageURL と同じ。createdAt（画像の行の created_at。投稿の画像なら posts.posted_at）が分かっていれば、
// 所有者と一緒に覚えておき、キャッシュにない画像を読むときにパーティションを絞り込むのに使う（#101）。ゼロ値なら覚えない。
func signedImageURLAt(user User, imageID string, createdAt time.Time) string {
	owner, ok := imageOwnerKeyCache.Get(imageID)
	if user.SecretKeyHash != "" {
		updated := owner
		if !ok || (owner.OwnerID == "" && user.ID != "") {
			updated = imageOwner{OwnerID: user.ID, SecretKeyHash: user.SecretKeyHash, CreatedAt: owner.CreatedAt}
		}
		if updated.CreatedAt.IsZero() {
			updated.CreatedAt = createdAt
		}
		if !ok || updated != owner {
			owner = updated
			imageOwnerKeyCache.Add(imageID, owner, 1, time.Time{})
		}
	}
	exp := time.Now().Add(imageURLLifetime).Unix()
	exp -= exp % imageURLExpGranularity
	expText := strconv.FormatInt(exp, 10)
	cacheKey := string(user.ImageKey) + "\x00" + imageID
	entry, ok := signedURLCache.Get(cacheKey)
	if !ok || entry.exp != exp {
		entry = signedURLEntry{exp: exp, sig: base64.RawURLEncoding.EncodeToString(imageSignature(user.ImageKey, imageID, exp))}
		signedURLCache.Add(cacheKey, entry, 1, time.Time{})
		// 取得の検証では、imageOwnerKeyCache の鍵で同じ計算をする。その鍵がこの user の鍵と同じなら、
		// 検証の結果は必ず成功なので、先に検証済みとして覚えておき、取得のときに HMAC を計算し直さない。
		if user.SecretKeyHash != "" && owner.SecretKeyHash == user.SecretKeyHash {
			verifiedSignatureCache.Add(verifiedSignatureKey(imageID, expText, entry.sig), struct{}{}, 1, time.Unix(exp, 0))
		}
	}
	query := url.Values{
		"exp": {expText},
		"sig": {entry.sig},
	}
	return "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
}
