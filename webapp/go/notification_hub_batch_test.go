package isureal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// #60 の、通知をまとめて引いて渡す処理と、LISTEN で起こされたときの処理のテスト（DB なし）。

type fakeBatchSource struct {
	mu          sync.Mutex
	windows     []string
	pending     map[string]getNotificationsResponse // userID -> 通知
	err         error
	windowCalls int
	batchCalls  [][]notificationKey
}

func (s *fakeBatchSource) ActiveWindowIDs(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windowCalls++
	return append([]string(nil), s.windows...), nil
}

func (s *fakeBatchSource) ActiveUserRegions(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (s *fakeBatchSource) PendingNotifications(_ context.Context, keys []notificationKey) ([]string, []*getNotificationsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchCalls = append(s.batchCalls, append([]notificationKey(nil), keys...))
	if s.err != nil {
		return nil, nil, s.err
	}
	results := make([]*getNotificationsResponse, len(keys))
	for i, key := range keys {
		if n, ok := s.pending[key.UserID]; ok && n.CreatedAt.After(key.ConnectedAt) {
			results[i] = &n
		}
	}
	return append([]string(nil), s.windows...), results, nil
}

func delivered(w *notificationWaiter) (getNotificationsResponse, bool) {
	select {
	case n := <-w.delivered:
		return n, true
	default:
		return getNotificationsResponse{}, false
	}
}

func TestNotificationHubBatchDelivers(t *testing.T) {
	base := time.Now()
	notification := getNotificationsResponse{ID: "n1", PostingWindowID: "w2", Region: "asia", CreatedAt: base.Add(time.Second)}
	source := &fakeBatchSource{windows: []string{"w1"}, pending: map[string]getNotificationsResponse{}}
	hub := newNotificationHub(source)
	withNotification := hub.subscribe("u1", "asia")
	withNotification.setConnectedAt(base)
	connectedLater := hub.subscribe("u1", "asia")
	connectedLater.setConnectedAt(base.Add(2 * time.Second))
	withoutNotification := hub.subscribe("u2", "asia")
	withoutNotification.setConnectedAt(base)
	notReady := hub.subscribe("u3", "asia")
	ctx := context.Background()

	hub.checkWindows(ctx)
	if len(source.batchCalls) != 1 || len(source.batchCalls[0]) != 3 {
		t.Fatalf("first check must fetch once for the 3 ready waiters, got %v", source.batchCalls)
	}
	// 接続した時刻がまだの接続は、自分で引かせない（#83）。時刻が決まったときにハブを起こす。
	if signaled(notReady) {
		t.Fatal("waiter without connectedAt must not be signaled to query by itself")
	}
	if !notReady.setConnectedAt(base) {
		t.Fatal("waiter that missed a batch must report it when connectedAt is set")
	}
	if notReady.setConnectedAt(base) {
		t.Fatal("missed flag must be cleared once reported")
	}

	source.windows = []string{"w2"}
	source.pending["u1"] = notification
	hub.checkWindows(ctx)
	if got, ok := delivered(withNotification); !ok || got != notification {
		t.Fatalf("expected %v to be delivered, got %v %v", notification, got, ok)
	}
	for name, w := range map[string]*notificationWaiter{"connectedLater": connectedLater, "withoutNotification": withoutNotification} {
		if _, ok := delivered(w); ok {
			t.Fatalf("%s: notification created before connecting must not be delivered", name)
		}
		if signaled(w) {
			t.Fatalf("%s: ready waiter without notification must keep waiting silently", name)
		}
	}

	// 一覧が変わらなければ、まとめて引かない。
	hub.checkWindows(ctx)
	if len(source.batchCalls) != 2 {
		t.Fatalf("unchanged windows must not fetch again, got %d calls", len(source.batchCalls))
	}
}

// まとめ取りが失敗しても、各接続に引かせず（N クエリにせず）、次の確認でまとめ取りをやり直すこと（#83）。
func TestNotificationHubBatchErrorRetries(t *testing.T) {
	base := time.Now()
	notification := getNotificationsResponse{ID: "n1", PostingWindowID: "w1", Region: "asia", CreatedAt: base.Add(time.Second)}
	source := &fakeBatchSource{windows: []string{"w1"}, pending: map[string]getNotificationsResponse{"u1": notification}, err: errors.New("db down")}
	hub := newNotificationHub(source)
	waiters := []*notificationWaiter{hub.subscribe("u1", "asia"), hub.subscribe("u2", "asia"), hub.subscribe("u3", "asia")}
	for _, w := range waiters {
		w.setConnectedAt(base)
	}
	ctx := context.Background()

	for i := 1; i < notificationBatchMaxFailures; i++ {
		hub.checkWindows(ctx)
		for j, w := range waiters {
			if signaled(w) {
				t.Fatalf("failure %d: waiter %d must not be signaled before %d failures", i, j, notificationBatchMaxFailures)
			}
		}
	}
	if len(source.batchCalls) != notificationBatchMaxFailures-1 {
		t.Fatalf("each check after a failure must retry the batch, got %d calls", len(source.batchCalls))
	}

	// 失敗の後は、一覧が同じでも次回の確認でまとめ取りをやり直し、届ける。
	source.err = nil
	hub.checkWindows(ctx)
	if got, ok := delivered(waiters[0]); !ok || got != notification {
		t.Fatalf("recovery must deliver by batch, got %v %v", got, ok)
	}
	for j, w := range waiters {
		if signaled(w) {
			t.Fatalf("waiter %d must not be signaled after recovery", j)
		}
	}
}

// まとめ取りが notificationBatchMaxFailures 回続けて失敗したら、以前と同じく各接続に引かせること。
// 知らせた後は数え直す。成功したら失敗の回数を0に戻す。
func TestNotificationHubBatchErrorFallsBackAfterRepeatedFailures(t *testing.T) {
	source := &fakeBatchSource{windows: []string{"w1"}, err: errors.New("db down")}
	hub := newNotificationHub(source)
	ready := hub.subscribe("u1", "asia")
	ready.setConnectedAt(time.Now())
	notReady := hub.subscribe("u2", "asia")
	ctx := context.Background()

	fail := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			hub.checkWindows(ctx)
		}
	}
	fail(notificationBatchMaxFailures - 1)
	if signaled(ready) {
		t.Fatal("must not fall back before the limit")
	}
	fail(1)
	if !signaled(ready) {
		t.Fatal("after repeated batch errors, ready waiters must be signaled to query by themselves")
	}
	if signaled(notReady) {
		t.Fatal("waiter without connectedAt cannot query yet; it must wait for its own wake")
	}
	fail(notificationBatchMaxFailures - 1)
	if signaled(ready) {
		t.Fatal("counter must restart after falling back")
	}
	fail(1)
	if !signaled(ready) {
		t.Fatal("must fall back again after another run of failures")
	}

	// 成功を挟むと、失敗の回数は0から数え直す。
	fail(notificationBatchMaxFailures - 1)
	source.err = nil
	hub.checkWindows(ctx)
	source.err = errors.New("db down")
	source.windows = []string{"w2"}
	fail(notificationBatchMaxFailures - 1)
	if signaled(ready) {
		t.Fatal("success must reset the failure counter")
	}
}

// 接続した時刻が決まる前にまとめ取りがあった接続は、時刻が決まったときにハブを起こし、
// 次のまとめ取りで通知を受け取ること（#83）。その接続は自分で引かない。
func TestNotificationHubLateConnectedAtJoinsBatch(t *testing.T) {
	base := time.Now()
	source := &fakeBatchSource{windows: []string{"w1"}, pending: map[string]getNotificationsResponse{}}
	hub := newNotificationHub(source)
	hub.windowPollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.run(ctx)
	batchCalls := func() int {
		source.mu.Lock()
		defer source.mu.Unlock()
		return len(source.batchCalls)
	}

	waiter := hub.subscribe("u1", "asia")
	notification := getNotificationsResponse{ID: "n1", PostingWindowID: "w2", Region: "asia", CreatedAt: base.Add(time.Second)}
	source.mu.Lock()
	source.windows = []string{"w2"}
	source.pending["u1"] = notification
	source.mu.Unlock()
	hub.notifyWindowChanged()
	deadline := time.Now().Add(time.Second)
	for batchCalls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("hub did not run the batch")
		}
		time.Sleep(time.Millisecond)
	}
	source.mu.Lock()
	if len(source.batchCalls[0]) != 0 {
		t.Fatalf("waiter without connectedAt must not be in the batch, got %v", source.batchCalls[0])
	}
	source.mu.Unlock()

	hub.setConnectedAt(waiter, base)
	select {
	case got := <-waiter.delivered:
		if got != notification {
			t.Fatalf("got %v, want %v", got, notification)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter whose connectedAt was set late did not receive the notification")
	}
	if signaled(waiter) {
		t.Fatal("waiter must not be told to query by itself")
	}

	// まとめ取りを逃していない接続は、時刻が決まってもハブを起こさない。
	before := batchCalls()
	other := hub.subscribe("u2", "asia")
	hub.setConnectedAt(other, base)
	time.Sleep(50 * time.Millisecond)
	if after := batchCalls(); after != before {
		t.Fatalf("waiter that missed nothing must not wake the hub, got %d extra calls", after-before)
	}
}

func TestNotificationHubWakeSkipsWindowQuery(t *testing.T) {
	base := time.Now()
	notification := getNotificationsResponse{ID: "n1", PostingWindowID: "w1", Region: "asia", CreatedAt: base.Add(time.Second)}
	source := &fakeBatchSource{windows: []string{"w1"}, pending: map[string]getNotificationsResponse{"u1": notification}}
	hub := newNotificationHub(source)
	waiter := hub.subscribe("u1", "asia")
	waiter.setConnectedAt(base)

	hub.checkWindowsNow(context.Background())
	if source.windowCalls != 0 {
		t.Fatalf("wake must not query the window list separately, got %d calls", source.windowCalls)
	}
	if got, ok := delivered(waiter); !ok || got != notification {
		t.Fatalf("wake must deliver pending notification, got %v %v", got, ok)
	}

	// まとめて引いたときの一覧を覚えているので、その後のポーリングで同じ一覧なら引き直さない。
	hub.checkWindows(context.Background())
	if len(source.batchCalls) != 1 {
		t.Fatalf("poll after wake with same windows must not fetch again, got %d calls", len(source.batchCalls))
	}
}

func TestNotificationHubRunWake(t *testing.T) {
	base := time.Now()
	source := &fakeBatchSource{windows: []string{"w1"}, pending: map[string]getNotificationsResponse{}}
	hub := newNotificationHub(source)
	hub.windowPollInterval = time.Hour
	waiter := hub.subscribe("u1", "asia")
	waiter.setConnectedAt(base)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.run(ctx)

	notification := getNotificationsResponse{ID: "n1", PostingWindowID: "w2", Region: "asia", CreatedAt: base.Add(time.Second)}
	source.mu.Lock()
	source.windows = []string{"w2"}
	source.pending["u1"] = notification
	source.mu.Unlock()
	start := time.Now()
	hub.notifyWindowChanged()
	select {
	case got := <-waiter.delivered:
		if got != notification {
			t.Fatalf("got %v, want %v", got, notification)
		}
	case <-time.After(time.Second):
		t.Fatal("wake did not deliver while polling is effectively off")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("wake took too long: %v", elapsed)
	}
}
