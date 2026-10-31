package isureal

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLRUCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newLRUCache[string, int](2)
	cache.Add("a", 1, 1, time.Time{})
	cache.Add("b", 2, 1, time.Time{})
	if _, ok := cache.Get("a"); !ok {
		t.Fatal("a should be cached")
	}
	cache.Add("c", 3, 1, time.Time{})
	if _, ok := cache.Get("b"); ok {
		t.Fatal("b should be evicted")
	}
	if v, ok := cache.Get("a"); !ok || v != 1 {
		t.Fatalf("a = %v, %v", v, ok)
	}
	if v, ok := cache.Get("c"); !ok || v != 3 {
		t.Fatalf("c = %v, %v", v, ok)
	}
}

func TestLRUCacheRespectsCostLimit(t *testing.T) {
	cache := newLRUCache[string, []byte](10)
	cache.Add("big", make([]byte, 11), 11, time.Time{})
	if _, ok := cache.Get("big"); ok {
		t.Fatal("entry larger than the limit should not be cached")
	}
	cache.Add("a", make([]byte, 6), 6, time.Time{})
	cache.Add("b", make([]byte, 6), 6, time.Time{})
	if _, ok := cache.Get("a"); ok {
		t.Fatal("a should be evicted to stay within the cost limit")
	}
	cache.Add("b", make([]byte, 4), 4, time.Time{})
	if cache.cost != 4 || cache.Len() != 1 {
		t.Fatalf("replacing should update cost: cost=%d len=%d", cache.cost, cache.Len())
	}
}

func TestLRUCacheExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := newLRUCache[string, int](10)
	cache.now = func() time.Time { return now }
	cache.Add("a", 1, 1, now.Add(2*time.Second))
	now = now.Add(time.Second)
	if _, ok := cache.Get("a"); !ok {
		t.Fatal("a should be valid before expiry")
	}
	now = now.Add(time.Second)
	if _, ok := cache.Get("a"); ok {
		t.Fatal("a should expire at expiresAt")
	}
	if cache.Len() != 0 {
		t.Fatal("expired entry should be removed")
	}
}

func TestLRUCacheReset(t *testing.T) {
	cache := newLRUCache[string, int](10)
	cache.Add("a", 1, 1, time.Time{})
	cache.Reset()
	if _, ok := cache.Get("a"); ok || cache.Len() != 0 || cache.cost != 0 {
		t.Fatal("reset should clear the cache")
	}
}

func TestImageValidityTTLWithinDeletionGrace(t *testing.T) {
	// 削除後5秒を過ぎたら取得できてはいけない。
	if imageValidityTTL > 3*time.Second {
		t.Fatalf("imageValidityTTL = %v, must be well within 5s", imageValidityTTL)
	}
}

func TestSignedImageURLIsVerifiableAndCached(t *testing.T) {
	resetImageCaches()
	hash := sha256.Sum256([]byte("secret"))
	user := User{ImageKey: hash[:]}
	before := time.Now()
	first := signedImageURL(user, "image-1")
	second := signedImageURL(user, "image-1")

	parsed, err := url.Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/api/images/image-1" {
		t.Fatalf("path = %s", parsed.Path)
	}
	exp, err := strconv.ParseInt(parsed.Query().Get("exp"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if exp > time.Now().Add(imageURLLifetime).Unix() || exp <= before.Add(imageURLLifetime).Unix()-imageURLExpGranularity {
		t.Fatalf("exp %d is outside (lifetime - granularity, lifetime]", exp)
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parsed.Query().Get("sig"))
	if err != nil {
		t.Fatal(err)
	}
	if string(sig) != string(imageSignature(user.ImageKey, "image-1", exp)) {
		t.Fatal("signature does not match imageSignature")
	}
	// 同じ exp の間は同じURLになる（分の境目をまたいだ場合を除く）。
	if first != second && !strings.Contains(second, "exp="+strconv.FormatInt(exp+imageURLExpGranularity, 10)) {
		t.Fatalf("cached URL mismatch: %s vs %s", first, second)
	}

	other := sha256.Sum256([]byte("other"))
	if signedImageURL(User{ImageKey: other[:]}, "image-1") == first {
		t.Fatal("different keys must not share a cached signature")
	}
}

func TestSignedImageURLRemembersOwnerKey(t *testing.T) {
	resetImageCaches()
	t.Cleanup(resetImageCaches)
	hash := sha256.Sum256([]byte("owner-secret"))
	user := User{SecretKeyHash: hex.EncodeToString(hash[:])}
	if err := user.setImageKey(); err != nil {
		t.Fatal(err)
	}
	signedImageURL(user, "image-1")
	got, ok := imageOwnerKeyCache.Get("image-1")
	if !ok || got != user.SecretKeyHash {
		t.Fatalf("owner key = %q, %v", got, ok)
	}

	// 鍵が無いユーザー（ゼロ値）では覚えない。
	signedImageURL(User{}, "image-2")
	if _, ok := imageOwnerKeyCache.Get("image-2"); ok {
		t.Fatal("empty secret_key_hash should not be cached")
	}
}

func TestCacheUploadedImageValidityStartsBeforeCommit(t *testing.T) {
	resetImageCaches()
	t.Cleanup(resetImageCaches)
	validatedAt := time.Now().Add(-imageValidityTTL - time.Millisecond)
	cacheUploadedImage("old", []byte{1, 2, 3}, validatedAt)
	if _, ok := imageObjectCache.Get("old"); !ok {
		t.Fatal("image data should be cached")
	}
	// 有効期限は validatedAt から数えるので、それより古い確認結果は使わない。
	if _, ok := imageValidityCache.Get("old"); ok {
		t.Fatal("validity must expire imageValidityTTL after validatedAt")
	}
	cacheUploadedImage("new", []byte{4}, time.Now())
	if obj, ok := imageObjectCache.Get("new"); !ok || obj.MediaType != "image/jpeg" || len(obj.Data) != 1 {
		t.Fatalf("cached object = %+v, %v", obj, ok)
	}
	if _, ok := imageValidityCache.Get("new"); !ok {
		t.Fatal("fresh upload should be cached as valid")
	}
}
