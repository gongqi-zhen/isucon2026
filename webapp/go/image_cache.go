package isureal

import (
	"container/list"
	"os"
	"strconv"
	"sync"
	"time"
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

var (
	// (署名鍵, imageID) -> 生成済みの署名。exp を丸めて同じ署名を使い回す。
	signedURLCache = newLRUCache[string, signedURLEntry](200_000)
	// 検証に成功した (imageID, exp, sig)。exp まで有効。
	verifiedSignatureCache = newLRUCache[string, struct{}](200_000)
	// imageID -> 所有者の secret_key_hash。所有者も secret_key_hash も変わらない。
	imageOwnerKeyCache = newLRUCache[string, string](500_000)
	// imageID -> 画像本体。id ごとに中身は変わらない。
	imageObjectCache = newLRUCache[string, cachedImageObject](envInt64("ISUREAL_IMAGE_CACHE_BYTES", 512<<20))
	// imageID -> 有効だった時刻から imageValidityTTL の間だけ有効とみなす。
	imageValidityCache = newLRUCache[string, struct{}](500_000)
)

func resetImageCaches() {
	signedURLCache.Reset()
	verifiedSignatureCache.Reset()
	imageOwnerKeyCache.Reset()
	imageObjectCache.Reset()
	imageValidityCache.Reset()
}

func envInt64(key string, defaultValue int64) int64 {
	if value, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil && value > 0 {
		return value
	}
	return defaultValue
}
