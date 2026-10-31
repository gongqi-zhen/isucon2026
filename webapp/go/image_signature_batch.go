package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math/bits"
	"strconv"
	"time"
)

// 画像の署名（imageSignature）は HMAC-SHA256 を1000回つなげるので、asia1（SHA の専用命令がない Xeon 8275CL）では
// 負荷走行中の CPU の45%を占めていた（#127）。署名は exp の丸め（60秒）ごとに作り直すので、
// Memories やエクスポートのように画像の多い応答では、1分ごとに数十〜数百枚をまとめて署名し直すことになる。
//
// ここでは、それを8本ずつまとめ、AVX-512VL で8本の連鎖を同時に進める（image_signature_amd64.s）。
// 1本ずつ計算した場合と結果は同じ（image_signature_batch_test.go）。署名の形式と exp の丸めは変えない。

// hmacChainLanes は同時に進める連鎖の数。
const hmacChainLanes = 8

// hmacChainState は8本の連鎖の状態。どれも [語][レーン] の並びで、語は SHA-256 の big endian の32ビット。
// ipad と opad は、鍵と ipad／opad の XOR の1ブロックを処理した後の SHA-256 の状態（hmac.New の Reset が覚えているもの）。
// value は直前の HMAC の値（32バイト）。アセンブリはこの並び（オフセット 0、256、512）を前提にしている。
type hmacChainState struct {
	ipad  [8][hmacChainLanes]uint32
	opad  [8][hmacChainLanes]uint32
	value [8][hmacChainLanes]uint32
}

var sha256IV = [8]uint32{0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19}

var sha256K = [64]uint32{
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
	0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
	0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
	0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
	0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
	0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
}

// sha256Block は SHA-256 の圧縮関数を1ブロック分だけ行う（state に足し込む）。鍵から ipad／opad の状態を作るのと、
// アセンブリの使えない環境での連鎖（hmacChainGeneric）に使う。
func sha256Block(state *[8]uint32, block *[16]uint32) {
	var w [64]uint32
	copy(w[:], block[:])
	for i := 16; i < 64; i++ {
		v1, v2 := w[i-2], w[i-15]
		s1 := bits.RotateLeft32(v1, -17) ^ bits.RotateLeft32(v1, -19) ^ (v1 >> 10)
		s0 := bits.RotateLeft32(v2, -7) ^ bits.RotateLeft32(v2, -18) ^ (v2 >> 3)
		w[i] = s1 + w[i-7] + s0 + w[i-16]
	}
	a, b, c, d, e, f, g, h := state[0], state[1], state[2], state[3], state[4], state[5], state[6], state[7]
	for i := range 64 {
		t1 := h + (bits.RotateLeft32(e, -6) ^ bits.RotateLeft32(e, -11) ^ bits.RotateLeft32(e, -25)) + ((e & f) ^ (^e & g)) + sha256K[i] + w[i]
		t2 := (bits.RotateLeft32(a, -2) ^ bits.RotateLeft32(a, -13) ^ bits.RotateLeft32(a, -22)) + ((a & b) ^ (a & c) ^ (b & c))
		h, g, f, e, d, c, b, a = g, f, e, d+t1, c, b, a, t1+t2
	}
	state[0] += a
	state[1] += b
	state[2] += c
	state[3] += d
	state[4] += e
	state[5] += f
	state[6] += g
	state[7] += h
}

// hmacPadStates は、crypto/hmac と同じ規則（鍵が64バイトを超えたら SHA-256 で縮め、64バイトまで0で埋める）で、
// ipad と opad の1ブロックを処理した後の状態を返す。
func hmacPadStates(key []byte) (ipad, opad [8]uint32) {
	if len(key) > sha256.BlockSize {
		sum := sha256.Sum256(key)
		key = sum[:]
	}
	var padded [sha256.BlockSize]byte
	copy(padded[:], key)
	var inner, outer [16]uint32
	for i := range inner {
		word := binary.BigEndian.Uint32(padded[4*i:])
		inner[i] = word ^ 0x36363636
		outer[i] = word ^ 0x5c5c5c5c
	}
	ipad, opad = sha256IV, sha256IV
	sha256Block(&ipad, &inner)
	sha256Block(&opad, &outer)
	return ipad, opad
}

// hmacChainGeneric は hmacChainX8AVX512 と同じ計算を Go で行う。value = HMAC(key, value) を iterations 回。
func hmacChainGeneric(st *hmacChainState, iterations int) {
	for lane := range hmacChainLanes {
		var value [8]uint32
		for i := range value {
			value[i] = st.value[i][lane]
		}
		for range iterations {
			for _, pad := range []*[8][hmacChainLanes]uint32{&st.ipad, &st.opad} {
				var state [8]uint32
				for i := range state {
					state[i] = pad[i][lane]
				}
				block := [16]uint32{8: 0x80000000, 15: (sha256.BlockSize + sha256.Size) * 8}
				copy(block[:8], value[:])
				sha256Block(&state, &block)
				value = state
			}
		}
		for i := range value {
			st.value[i][lane] = value[i]
		}
	}
}

// imageSignatureRequest は、まとめて署名する1枚分。
type imageSignatureRequest struct {
	key     []byte
	imageID string
}

// imageSignatures は、それぞれの imageSignature(key, imageID, exp) と同じ値を返す。
// hmacChainX8 が使えれば8本ずつ同時に計算し、使えなければ1本ずつ imageSignature を呼ぶ。
func imageSignatures(requests []imageSignatureRequest, exp int64) [][]byte {
	sigs := make([][]byte, len(requests))
	if !hmacChainX8Available || len(requests) < 2 {
		for i, request := range requests {
			sigs[i] = imageSignature(request.key, request.imageID, exp)
		}
		return sigs
	}
	expPrefix := strconv.FormatInt(exp, 10) + ":"
	var st hmacChainState
	for start := 0; start < len(requests); start += hmacChainLanes {
		batch := requests[start:min(start+hmacChainLanes, len(requests))]
		for lane := range hmacChainLanes {
			// 余ったレーンは先頭と同じものを計算し、結果は捨てる。
			request := batch[0]
			if lane < len(batch) {
				request = batch[lane]
			}
			// 1回目は入力の長さが画像ごとに違う（"exp:imageID"）ので、crypto/hmac で計算する。2回目からは入力が32バイトで揃う。
			mac := hmac.New(sha256.New, request.key)
			mac.Write([]byte(expPrefix + request.imageID))
			first := mac.Sum(nil)
			ipad, opad := hmacPadStates(request.key)
			for i := range 8 {
				st.ipad[i][lane] = ipad[i]
				st.opad[i][lane] = opad[i]
				st.value[i][lane] = binary.BigEndian.Uint32(first[4*i:])
			}
		}
		hmacChainX8(&st, imageSignatureIterations-1)
		for lane := range batch {
			sig := make([]byte, sha256.Size)
			for i := range 8 {
				binary.BigEndian.PutUint32(sig[4*i:], st.value[i][lane])
			}
			sigs[start+lane] = sig
		}
	}
	return sigs
}

// imageURLRequest は、presignImageURLs に渡す1枚分。signedImageURLAt の引数と同じ。
type imageURLRequest struct {
	user      User
	imageID   string
	createdAt time.Time
}

// presignImageURLs は、これから signedImageURLAt で URL を作る画像の署名を、まとめて計算して signedURLCache に入れておく。
// 後で呼ぶ signedImageURLAt はキャッシュに当たるので、出てくる URL と、キャッシュ（所有者、検証済みの署名）の中身は、
// presignImageURLs を呼ばない場合と同じになる。途中で exp の丸めの境目をまたいだら、signedImageURLAt が今まで通り計算し直す。
func presignImageURLs(requests []imageURLRequest) {
	if !hmacChainX8Available {
		return
	}
	exp, expText := currentImageURLExp()
	type pending struct {
		user    User
		imageID string
		owner   imageOwner
	}
	var misses []pending
	var signRequests []imageSignatureRequest
	seen := make(map[string]struct{})
	for _, request := range requests {
		owner := rememberImageOwner(request.user, request.imageID, request.createdAt)
		cacheKey := signedURLCacheKey(request.user, request.imageID)
		if entry, ok := signedURLCache.Get(cacheKey); ok && entry.exp == exp {
			continue
		}
		if _, ok := seen[cacheKey]; ok {
			continue
		}
		seen[cacheKey] = struct{}{}
		misses = append(misses, pending{user: request.user, imageID: request.imageID, owner: owner})
		signRequests = append(signRequests, imageSignatureRequest{key: request.user.ImageKey, imageID: request.imageID})
	}
	if len(misses) < 2 {
		// 1枚なら、まとめても速くならない。signedImageURLAt に任せる。
		return
	}
	sigs := imageSignatures(signRequests, exp)
	for i, miss := range misses {
		entry := signedURLEntry{exp: exp, sig: base64.RawURLEncoding.EncodeToString(sigs[i])}
		storeSignedURL(miss.user, miss.imageID, miss.owner, entry, expText)
	}
}

// hmacChainX8 は、8本の連鎖を iterations 回進める。hmacChainX8Available が true のときだけ、まとめて計算する（遅い環境では1本ずつの方が速い）。
// amd64 で AVX-512VL があれば、init（image_signature_amd64.go）でアセンブリに差し替える。
var (
	hmacChainX8          = hmacChainGeneric
	hmacChainX8Available = false
)
