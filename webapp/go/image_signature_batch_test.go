package isureal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"
)

// signatureTestKeys は、本番と同じ32バイトの鍵に加えて、crypto/hmac の規則（64バイトを超えたら縮める）を通る長さも含む。
func signatureTestKeys() [][]byte {
	keys := [][]byte{nil, make([]byte, sha256.Size), bytes.Repeat([]byte{0xff}, 64), bytes.Repeat([]byte{0x5a}, 65), []byte("short")}
	for i := range 20 {
		hash := sha256.Sum256(fmt.Appendf(nil, "secret-%d", i))
		keys = append(keys, hash[:])
	}
	return keys
}

// ISUREAL_TEST_HMAC_X8_GENERIC=1 なら、アセンブリの使えない環境（手元の Mac など）でも、まとめて署名する経路
// （presignImageURLs）を Go の実装で通す。同一性テスト（memories_equivalence_test.go など）をこの経路で流すのに使う。
func init() {
	if os.Getenv("ISUREAL_TEST_HMAC_X8_GENERIC") == "1" && !hmacChainX8Available {
		hmacChainX8, hmacChainX8Available = hmacChainGeneric, true
	}
}

// useHMACChainX8 は、テストの間だけ、まとめて署名する経路を impl で有効にする。
func useHMACChainX8(t testing.TB, impl func(*hmacChainState, int)) {
	savedImpl, savedAvailable := hmacChainX8, hmacChainX8Available
	hmacChainX8, hmacChainX8Available = impl, true
	t.Cleanup(func() { hmacChainX8, hmacChainX8Available = savedImpl, savedAvailable })
}

// hmacChainImpls は、この環境で試せる実装。アセンブリは AVX-512VL のある amd64 でだけ試せる。
func hmacChainImpls() map[string]func(*hmacChainState, int) {
	impls := map[string]func(*hmacChainState, int){"generic": hmacChainGeneric}
	if hmacChainX8Available {
		impls["x8"] = hmacChainX8
	}
	return impls
}

func TestHMACPadStatesMatchSingleBlock(t *testing.T) {
	for _, key := range signatureTestKeys() {
		// 1回だけの HMAC（入力32バイト）を、ipad／opad の状態から作ったものと crypto/hmac とで比べる。
		value := sha256.Sum256(key)
		var st hmacChainState
		ipad, opad := hmacPadStates(key)
		for lane := range hmacChainLanes {
			for i := range 8 {
				st.ipad[i][lane], st.opad[i][lane] = ipad[i], opad[i]
				st.value[i][lane] = uint32(value[4*i])<<24 | uint32(value[4*i+1])<<16 | uint32(value[4*i+2])<<8 | uint32(value[4*i+3])
			}
		}
		hmacChainGeneric(&st, 1)
		want := referenceImageSignatureFrom(key, value[:], 1)
		for lane := range hmacChainLanes {
			got := make([]byte, 0, sha256.Size)
			for i := range 8 {
				got = append(got, byte(st.value[i][lane]>>24), byte(st.value[i][lane]>>16), byte(st.value[i][lane]>>8), byte(st.value[i][lane]))
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("key %x lane %d: got %x, want %x", key, lane, got, want)
			}
		}
	}
}

// referenceImageSignatureFrom は、value から HMAC を iterations 回つなげる（referenceImageSignature の途中から）。
func referenceImageSignatureFrom(key, value []byte, iterations int) []byte {
	for range iterations {
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		value = mac.Sum(nil)
	}
	return value
}

func TestImageSignaturesMatchImageSignature(t *testing.T) {
	impls := hmacChainImpls()
	keys := signatureTestKeys()
	rng := rand.New(rand.NewPCG(1, 2))
	// 8の倍数、端数、1枚、0枚を試す。同じ鍵や同じ imageID が混ざってもよい。
	for _, n := range []int{0, 1, 2, 7, 8, 9, 17, 40} {
		requests := make([]imageSignatureRequest, n)
		for i := range requests {
			requests[i] = imageSignatureRequest{
				key:     keys[rng.IntN(len(keys))],
				imageID: []string{"", "image-1", fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-%012d", rng.IntN(1000)), "画像/with?query&x=1"}[rng.IntN(4)],
			}
		}
		for _, exp := range []int64{0, 1790000040} {
			want := make([][]byte, n)
			for i, request := range requests {
				want[i] = imageSignature(request.key, request.imageID, exp)
			}
			for name, impl := range impls {
				useHMACChainX8(t, impl)
				got := imageSignatures(requests, exp)
				for i := range requests {
					if !bytes.Equal(got[i], want[i]) {
						t.Fatalf("%s n=%d exp=%d [%d] (%x, %q): got %x, want %x", name, n, exp, i, requests[i].key, requests[i].imageID, got[i], want[i])
					}
				}
			}
		}
	}
}

// presignImageURLs を先に呼んでも、signedImageURLAt が返す URL と、キャッシュ（所有者、検証済みの署名）の中身が同じになること。
func TestPresignImageURLsKeepsURLsAndCaches(t *testing.T) {
	makeUser := func(i int) User {
		user := User{ID: fmt.Sprintf("user-%d", i), SecretKeyHash: fmt.Sprintf("%064x", i+1)}
		if err := user.setImageKey(); err != nil {
			t.Fatal(err)
		}
		return user
	}
	var requests []imageURLRequest
	postedAt := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	for i := range 21 {
		user := makeUser(i % 5)
		requests = append(requests, imageURLRequest{user: user, imageID: fmt.Sprintf("image-%d", i), createdAt: postedAt.Add(time.Duration(i) * time.Second)})
	}
	// 同じ画像が2回出てくる場合と、所有者でない利用者の鍵で署名する場合も含める。
	requests = append(requests, requests[3], imageURLRequest{user: User{ID: "", SecretKeyHash: ""}, imageID: "image-4"})

	build := func() []string {
		urls := make([]string, len(requests))
		for i, request := range requests {
			urls[i] = signedImageURLAt(request.user, request.imageID, request.createdAt)
		}
		return urls
	}
	snapshot := func() map[string]imageOwner {
		owners := make(map[string]imageOwner)
		for _, request := range requests {
			owner, _ := imageOwnerKeyCache.Get(request.imageID)
			owners[request.imageID] = owner
		}
		return owners
	}

	impls := hmacChainImpls()
	resetImageCaches()
	t.Cleanup(resetImageCaches)
	want := build()
	wantOwners := snapshot()
	wantVerified := verifiedSignatureCache.Len()

	for name, impl := range impls {
		useHMACChainX8(t, impl)
		resetImageCaches()
		presignImageURLs(requests)
		misses := signedURLCache.Stats().Misses
		got := build()
		if after := signedURLCache.Stats().Misses; after != misses {
			// 境目（60秒）をまたいだときだけ起きる。
			t.Logf("%s: signedImageURLAt recomputed %d signatures (exp boundary?)", name, after-misses)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s [%d]: got %q, want %q", name, i, got[i], want[i])
			}
		}
		gotOwners := snapshot()
		for id, owner := range wantOwners {
			if gotOwners[id] != owner {
				t.Fatalf("%s owner of %s: got %+v, want %+v", name, id, gotOwners[id], owner)
			}
		}
		if got := verifiedSignatureCache.Len(); got != wantVerified {
			t.Fatalf("%s verified signatures: got %d, want %d", name, got, wantVerified)
		}
		for i, request := range requests {
			exp, expText := currentImageURLExp()
			entry, _ := signedURLCache.Get(signedURLCacheKey(request.user, request.imageID))
			if entry.exp != exp {
				continue
			}
			_, ok := verifiedSignatureCache.Get(verifiedSignatureKey(request.imageID, expText, entry.sig))
			owner := gotOwners[request.imageID]
			if wantOK := request.user.SecretKeyHash != "" && owner.SecretKeyHash == request.user.SecretKeyHash; ok != wantOK {
				t.Fatalf("%s [%d] verified = %v, want %v", name, i, ok, wantOK)
			}
		}
	}
}

func BenchmarkImageSignatures(b *testing.B) {
	keys := signatureTestKeys()
	requests := make([]imageSignatureRequest, 64)
	for i := range requests {
		requests[i] = imageSignatureRequest{key: keys[5+i%10], imageID: fmt.Sprintf("0190a1b2-c3d4-7e5f-8a9b-%012d", i)}
	}
	impls := hmacChainImpls()
	b.Run("single", func(b *testing.B) {
		for b.Loop() {
			for _, request := range requests {
				imageSignature(request.key, request.imageID, 1790000040)
			}
		}
	})
	for name, impl := range impls {
		b.Run(name, func(b *testing.B) {
			useHMACChainX8(b, impl)
			for b.Loop() {
				imageSignatures(requests, 1790000040)
			}
		})
	}
}
