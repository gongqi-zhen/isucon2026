package isureal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strconv"
	"testing"
)

// 変更前の実装（毎回 hmac.New する）。imageSignature の結果がこれと同じであることを確かめる（#71）。
func referenceImageSignature(key []byte, imageID string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	for range imageSignatureIterations {
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		value = mac.Sum(nil)
	}
	return value
}

func TestImageSignatureMatchesReference(t *testing.T) {
	keys := [][]byte{make([]byte, sha256.Size)}
	for _, secret := range []string{"secret", "other", "こんにちは"} {
		hash := sha256.Sum256([]byte(secret))
		keys = append(keys, hash[:])
	}
	imageIDs := []string{"", "image-1", "0190a1b2-c3d4-7e5f-8a9b-000000000002", "画像/with?query&x=1"}
	exps := []int64{0, 60, 1790000000, 1790000059}
	for _, key := range keys {
		for _, imageID := range imageIDs {
			for _, exp := range exps {
				got := imageSignature(key, imageID, exp)
				want := referenceImageSignature(key, imageID, exp)
				if !bytes.Equal(got, want) {
					t.Fatalf("imageSignature(%x, %q, %d) = %x, want %x", key, imageID, exp, got, want)
				}
			}
		}
	}
}

// 変更前の実装（url.Values を Encode する）。signedImageURLString の結果がこれと同じであることを確かめる（#112）。
func referenceSignedImageURLString(imageID, expText, sig string) string {
	query := url.Values{
		"exp": {expText},
		"sig": {sig},
	}
	return "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
}

func TestSignedImageURLStringMatchesReference(t *testing.T) {
	imageIDs := []string{"", "image-1", "0190a1b2-c3d4-7e5f-8a9b-000000000002", "画像/with?query&x=1", "a b+c%d"}
	exps := []string{"0", "60", "1790000000", "1790000059"}
	sigs := []string{""}
	for _, secret := range []string{"secret", "other", "こんにちは"} {
		hash := sha256.Sum256([]byte(secret))
		sigs = append(sigs, base64.RawURLEncoding.EncodeToString(imageSignature(hash[:], "image-1", 1790000000)))
	}
	// RawURLEncoding の全文字（A-Z、a-z、0-9、-、_）を含む署名。
	sigs = append(sigs, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_")
	for _, imageID := range imageIDs {
		for _, exp := range exps {
			for _, sig := range sigs {
				got := signedImageURLString(imageID, exp, sig)
				want := referenceSignedImageURLString(imageID, exp, sig)
				if got != want {
					t.Fatalf("signedImageURLString(%q, %q, %q) = %q, want %q", imageID, exp, sig, got, want)
				}
			}
		}
	}
}

func TestSignedImageURLMatchesReferenceFormat(t *testing.T) {
	resetImageCaches()
	t.Cleanup(resetImageCaches)
	hash := sha256.Sum256([]byte("owner-secret"))
	user := User{ID: "owner-1", SecretKeyHash: hex.EncodeToString(hash[:])}
	if err := user.setImageKey(); err != nil {
		t.Fatal(err)
	}
	for _, imageID := range []string{"0190a1b2-c3d4-7e5f-8a9b-000000000002", "画像/with?query&x=1"} {
		got := signedImageURL(user, imageID)
		parsed, err := url.Parse(got)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		want := referenceSignedImageURLString(imageID, query.Get("exp"), query.Get("sig"))
		if got != want {
			t.Fatalf("signedImageURL(%q) = %q, want %q", imageID, got, want)
		}
		if parsed.Path != "/api/images/"+imageID {
			t.Fatalf("path of %q = %q, want %q", got, parsed.Path, "/api/images/"+imageID)
		}
	}
}

func TestSignedImageURLMarksOwnSignatureVerified(t *testing.T) {
	resetImageCaches()
	t.Cleanup(resetImageCaches)
	ownerUser := func(id, secret string) User {
		hash := sha256.Sum256([]byte(secret))
		user := User{ID: id, SecretKeyHash: hex.EncodeToString(hash[:])}
		if err := user.setImageKey(); err != nil {
			t.Fatal(err)
		}
		return user
	}
	verified := func(rawURL string) bool {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		_, ok := verifiedSignatureCache.Get(verifiedSignatureKey(parsed.Path[len("/api/images/"):], query.Get("exp"), query.Get("sig")))
		return ok
	}

	owner := ownerUser("owner-1", "owner-secret")
	if !verified(signedImageURL(owner, "image-1")) {
		t.Fatal("a signature made with the owner's key should be remembered as verified")
	}

	// 検証で使う鍵（imageOwnerKeyCache）と違う鍵で作った署名は、検証済みにしない。
	if verified(signedImageURL(ownerUser("someone-else", "other-secret"), "image-1")) {
		t.Fatal("a signature made with a key other than the cached owner's must not be marked verified")
	}

	// 鍵の元（secret_key_hash）が分からないユーザーでは、検証済みにしない。
	hash := sha256.Sum256([]byte("key-only"))
	if verified(signedImageURL(User{ImageKey: hash[:]}, "image-2")) {
		t.Fatal("a user without secret_key_hash must not mark signatures verified")
	}
}

func BenchmarkImageSignature(b *testing.B) {
	key := sha256.Sum256([]byte("secret"))
	for b.Loop() {
		imageSignature(key[:], "0190a1b2-c3d4-7e5f-8a9b-000000000002", 1790000000)
	}
}

func BenchmarkReferenceImageSignature(b *testing.B) {
	key := sha256.Sum256([]byte("secret"))
	for b.Loop() {
		referenceImageSignature(key[:], "0190a1b2-c3d4-7e5f-8a9b-000000000002", 1790000000)
	}
}
