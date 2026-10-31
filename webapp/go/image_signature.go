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

func imageSignature(key []byte, imageID string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	for range imageSignatureIterations {
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		value = mac.Sum(nil)
	}
	return value
}

// user は画像の所有者であること（投稿は作者、RealMoji はリアクションした人、プロフィール画像は本人）。
// 署名に使う所有者の鍵をここで覚えておき、画像の取得時に所有者を DB に問い合わせる往復を省く。
func signedImageURL(user User, imageID string) string {
	if _, ok := imageOwnerKeyCache.Get(imageID); !ok && user.SecretKeyHash != "" {
		imageOwnerKeyCache.Add(imageID, user.SecretKeyHash, 1, time.Time{})
	}
	exp := time.Now().Add(imageURLLifetime).Unix()
	exp -= exp % imageURLExpGranularity
	cacheKey := string(user.ImageKey) + "\x00" + imageID
	entry, ok := signedURLCache.Get(cacheKey)
	if !ok || entry.exp != exp {
		entry = signedURLEntry{exp: exp, sig: base64.RawURLEncoding.EncodeToString(imageSignature(user.ImageKey, imageID, exp))}
		signedURLCache.Add(cacheKey, entry, 1, time.Time{})
	}
	query := url.Values{
		"exp": {strconv.FormatInt(entry.exp, 10)},
		"sig": {entry.sig},
	}
	return "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
}
