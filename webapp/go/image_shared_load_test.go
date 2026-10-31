package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

// 同じ画像の同時のリクエストは、1回の読み込みにまとまり、全員が同じ sentAt を受け取る（#101）。
func TestLoadImageSharedMergesConcurrentLoads(t *testing.T) {
	var group singleflight.Group
	var calls atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) (string, error) {
		calls.Add(1)
		<-release
		return "data", nil
	}
	const n = 8
	type result struct {
		value  string
		sentAt time.Time
		err    error
	}
	results := make(chan result, n)
	var started sync.WaitGroup
	for range n {
		started.Add(1)
		go func() {
			started.Done()
			value, sentAt, err := loadImageShared(context.Background(), &group, "image-1", load)
			results <- result{value, sentAt, err}
		}()
	}
	started.Wait()
	// 全員が DoChan に入るまで少し待ってから、読み込みを終わらせる。
	time.Sleep(50 * time.Millisecond)
	close(release)
	var sentAt time.Time
	for i := range n {
		r := <-results
		if r.err != nil || r.value != "data" {
			t.Fatalf("result %d = %+v", i, r)
		}
		if i == 0 {
			sentAt = r.sentAt
		} else if !r.sentAt.Equal(sentAt) {
			t.Fatalf("waiters must reuse the shared sentAt: %v != %v", r.sentAt, sentAt)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("load called %d times, want 1", got)
	}

	// 別の画像はまとめない。
	if _, _, err := loadImageShared(context.Background(), &group, "image-2", func(context.Context) (string, error) {
		calls.Add(1)
		return "other", nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("a different key must load on its own: calls = %d", got)
	}
}

// 届いた時刻より imageValidityTTL 以上前に送ったクエリの結果は使わず、自分で引き直す。
// 削除の5秒後に届いたリクエストが、削除の前に送ったクエリの結果で返ることはない。
func TestLoadImageSharedReloadsWhenSharedQueryIsTooOld(t *testing.T) {
	var group singleflight.Group
	var calls atomic.Int32
	release := make(chan struct{})
	leaderDone := make(chan time.Time, 1)
	go func() {
		_, sentAt, _ := loadImageShared(context.Background(), &group, "slow", func(context.Context) (int32, error) {
			n := calls.Add(1)
			<-release
			return n, nil
		})
		leaderDone <- sentAt
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(imageValidityTTL + 50*time.Millisecond)
	arrivedAt := time.Now()
	lateDone := make(chan struct{})
	var lateValue int32
	var lateSentAt time.Time
	go func() {
		defer close(lateDone)
		lateValue, lateSentAt, _ = loadImageShared(context.Background(), &group, "slow", func(context.Context) (int32, error) {
			return calls.Add(1), nil
		})
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-lateDone
	leaderSentAt := <-leaderDone
	if lateValue != 2 || calls.Load() != 2 {
		t.Fatalf("late request should load on its own: value = %d, calls = %d", lateValue, calls.Load())
	}
	if lateSentAt.Before(arrivedAt) || !leaderSentAt.Before(arrivedAt) {
		t.Fatalf("sentAt: late = %v, leader = %v, arrived = %v", lateSentAt, leaderSentAt, arrivedAt)
	}
}

// 待っているリクエストが切れても、まとめた読み込みは止めない（ほかのリクエストが待っているため）。
// 切れたリクエストには ctx のエラーを返す。
func TestLoadImageSharedSurvivesCancelledCaller(t *testing.T) {
	var group singleflight.Group
	release := make(chan struct{})
	loadErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := loadImageShared(ctx, &group, "image", func(loadCtx context.Context) (string, error) {
			<-release
			loadErr <- loadCtx.Err()
			return "data", nil
		})
		firstDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	secondDone := make(chan string, 1)
	go func() {
		value, _, err := loadImageShared(context.Background(), &group, "image", func(context.Context) (string, error) {
			return "", errors.New("must not run")
		})
		if err != nil {
			value = "error: " + err.Error()
		}
		secondDone <- value
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: err = %v", err)
	}
	close(release)
	if err := <-loadErr; err != nil {
		t.Fatalf("shared load must not be cancelled by the first caller: %v", err)
	}
	if value := <-secondDone; value != "data" {
		t.Fatalf("waiter got %q", value)
	}
}

// 投稿の画像の署名付き URL を作るとき、画像の created_at（posted_at）を所有者と一緒に覚える。
// すでに覚えている所有者や created_at は変えない。
func TestSignedImageURLAtRemembersCreatedAt(t *testing.T) {
	resetImageCaches()
	t.Cleanup(resetImageCaches)
	hash := sha256.Sum256([]byte("owner-secret"))
	user := User{ID: "owner-1", SecretKeyHash: hex.EncodeToString(hash[:])}
	if err := user.setImageKey(); err != nil {
		t.Fatal(err)
	}
	postedAt := time.Date(2026, 10, 31, 12, 0, 0, 123000, time.UTC)

	signedImageURL(user, "image-1")
	if owner, _ := imageOwnerKeyCache.Get("image-1"); !owner.CreatedAt.IsZero() || owner.createdAtArg() != nil {
		t.Fatalf("created_at must be unknown without a post: %+v", owner)
	}
	signedImageURLAt(user, "image-1", postedAt)
	owner, _ := imageOwnerKeyCache.Get("image-1")
	if owner.OwnerID != user.ID || owner.SecretKeyHash != user.SecretKeyHash || !owner.CreatedAt.Equal(postedAt) {
		t.Fatalf("owner = %+v", owner)
	}
	if arg := owner.createdAtArg(); arg == nil || !arg.Equal(postedAt) {
		t.Fatalf("createdAtArg = %v", arg)
	}
	signedImageURLAt(user, "image-1", postedAt.Add(time.Hour))
	if owner, _ := imageOwnerKeyCache.Get("image-1"); !owner.CreatedAt.Equal(postedAt) {
		t.Fatalf("created_at must not be overwritten: %+v", owner)
	}

	// DB から読んだ所有者（loadImageOwnerKey）の created_at も残す。
	known := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	imageOwnerKeyCache.Add("image-2", imageOwner{OwnerID: user.ID, SecretKeyHash: user.SecretKeyHash, CreatedAt: known}, 1, time.Time{})
	signedImageURLAt(user, "image-2", postedAt)
	if owner, _ := imageOwnerKeyCache.Get("image-2"); !owner.CreatedAt.Equal(known) {
		t.Fatalf("created_at from the DB must be kept: %+v", owner)
	}
}
