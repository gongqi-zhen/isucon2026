package isureal

import (
	"container/list"
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// lruCache はコスト（件数やバイト数）の合計に上限を持つ LRU キャッシュ。
// expiresAt がゼロ値のエントリは期限なしとして扱う。
type lruCache[K comparable, V any] struct {
	mu      sync.Mutex
	maxCost int64
	cost    int64
	ll      *list.List
	items   map[K]*list.Element
	now     func() time.Time
}

type lruEntry[K comparable, V any] struct {
	key       K
	value     V
	cost      int64
	expiresAt time.Time
}

func newLRUCache[K comparable, V any](maxCost int64) *lruCache[K, V] {
	return &lruCache[K, V]{maxCost: maxCost, ll: list.New(), items: map[K]*list.Element{}, now: time.Now}
}

func (c *lruCache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	element, ok := c.items[key]
	if !ok {
		return zero, false
	}
	entry := element.Value.(*lruEntry[K, V])
	if !entry.expiresAt.IsZero() && !c.now().Before(entry.expiresAt) {
		c.removeElement(element)
		return zero, false
	}
	c.ll.MoveToFront(element)
	return entry.value, true
}

func (c *lruCache[K, V]) Add(key K, value V, cost int64, expiresAt time.Time) {
	if cost > c.maxCost {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		c.removeElement(element)
	}
	c.items[key] = c.ll.PushFront(&lruEntry[K, V]{key: key, value: value, cost: cost, expiresAt: expiresAt})
	c.cost += cost
	for c.cost > c.maxCost {
		c.removeElement(c.ll.Back())
	}
}

func (c *lruCache[K, V]) Remove(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		c.removeElement(element)
	}
}

func (c *lruCache[K, V]) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = map[K]*list.Element{}
	c.cost = 0
}

func (c *lruCache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *lruCache[K, V]) removeElement(element *list.Element) {
	entry := c.ll.Remove(element).(*lruEntry[K, V])
	delete(c.items, entry.key)
	c.cost -= entry.cost
}

// 画像の有効性（削除・差し替え・退会の反映）をキャッシュする時間。
// 削除後5秒を過ぎたら取得できてはいけないため、十分に短くする。
const imageValidityTTL = 2 * time.Second

type cachedImageObject struct {
	Data      []byte
	MediaType string
}

// imageOwner は画像の所有者。OwnerID が空なら、所有者の id は分かっていない。
type imageOwner struct {
	OwnerID       string `db:"owner_id"`
	SecretKeyHash string `db:"secret_key_hash"`
	// CreatedAt は画像の行の created_at（パーティションのキー）。ゼロ値なら分かっていない（#101）。
	// 投稿の画像は posts.posted_at と同じ値で保存される（#40）。
	CreatedAt time.Time `db:"created_at"`
}

// createdAtArg は、CreatedAt を SQL の引数にする。分かっていなければ NULL にする。
func (owner imageOwner) createdAtArg() *time.Time {
	if owner.CreatedAt.IsZero() {
		return nil
	}
	return &owner.CreatedAt
}

var (
	// (署名鍵, imageID) -> 生成済みの署名。exp を丸めて同じ署名を使い回す。
	signedURLCache = newLRUCache[string, signedURLEntry](200_000)
	// 検証に成功した (imageID, exp, sig)。exp まで有効。
	verifiedSignatureCache = newLRUCache[string, struct{}](200_000)
	// imageID -> 所有者の id と secret_key_hash。所有者も secret_key_hash も変わらない。
	imageOwnerKeyCache = newLRUCache[string, imageOwner](500_000)
	// imageID -> 画像本体。id ごとに中身は変わらない。
	imageObjectCache = newLRUCache[string, cachedImageObject](envInt64("ISUREAL_IMAGE_CACHE_BYTES", 512<<20))
	// imageID -> 有効だった時刻から imageValidityTTL の間だけ有効とみなす。
	imageValidityCache = newLRUCache[string, struct{}](500_000)
)

// アップロードを受け付けたプロセスで、確定した画像をそのままキャッシュに入れる。投稿直後の取得で DB を引かずに済む。
// COMMIT が成功した後に呼ぶこと。validatedAt は COMMIT の前に取った時刻にする。
// COMMIT の時点で画像は有効なので、そこから imageValidityTTL の間は有効とみなしても、削除の反映は TTL を超えて遅れない。
// 有効である理由（投稿や持ち主）が分からないので、無効化の一括検知（imageValidity）には入れない。TTL を過ぎたら DB で確かめる。
func cacheUploadedImage(imageID string, data []byte, validatedAt time.Time) {
	imageObjectCache.Add(imageID, cachedImageObject{Data: data, MediaType: "image/jpeg"}, int64(len(data)), time.Time{})
	imageValidityCache.Add(imageID, struct{}{}, 1, validatedAt.Add(imageValidityTTL))
}

// 画像の DB からの読み込みを、画像の ID ごとにまとめる（#101）。
// 同じ画像を同時に取りにきたリクエストは、先に送ったクエリの結果を待って使う。
// europe と america では1回の読み込みに RTT とスロースタートの分（約 140〜520ms）がかかるので、その間に届いた分を DB に送らずに済む。
var (
	imageObjectLoads   singleflight.Group
	imageValidityLoads singleflight.Group
	// まとめたクエリは、最初のリクエストが切れても止めない（待っている別のリクエストのため）。その上限の時間。
	imageSharedLoadTimeout = 10 * time.Second
)

type sharedImageLoad[T any] struct {
	value  T
	sentAt time.Time
}

// loadImageShared は、group の key ごとに load を1回だけ実行し、その結果と、クエリを送る直前の時刻（sentAt）を返す。
// 有効性の結果は sentAt から数える（markImageValid）。待っていた側も、まとめたクエリの sentAt をそのまま使う。
//
// 「削除の5秒後には取得できない」を保つため、このリクエストが届いた時刻より imageValidityTTL 以上前に送ったクエリの結果は使わず、
// 自分で引き直す。送った時刻がそれより後なら、届く5秒前までにコミットした削除は、そのクエリのスナップショットに含まれる。
// （まとめなかった場合は、届いた後に送ったクエリの結果を使っていた。）
func loadImageShared[T any](ctx context.Context, group *singleflight.Group, key string, load func(context.Context) (T, error)) (T, time.Time, error) {
	arrivedAt := time.Now()
	results := group.DoChan(key, func() (any, error) {
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), imageSharedLoadTimeout)
		defer cancel()
		sentAt := time.Now()
		value, err := load(loadCtx)
		return sharedImageLoad[T]{value: value, sentAt: sentAt}, err
	})
	var zero T
	select {
	case <-ctx.Done():
		return zero, time.Time{}, ctx.Err()
	case result := <-results:
		shared, _ := result.Val.(sharedImageLoad[T])
		if shared.sentAt.Before(arrivedAt.Add(-imageValidityTTL)) {
			sentAt := time.Now()
			value, err := load(ctx)
			return value, sentAt, err
		}
		return shared.value, shared.sentAt, result.Err
	}
}

func resetImageCaches() {
	signedURLCache.Reset()
	verifiedSignatureCache.Reset()
	imageOwnerKeyCache.Reset()
	imageObjectCache.Reset()
	imageValidityCache.Reset()
	imageValidity.reset()
}

func envInt64(key string, defaultValue int64) int64 {
	if value, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil && value > 0 {
		return value
	}
	return defaultValue
}
