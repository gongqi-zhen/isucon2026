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
	if !signaled(notReady) {
		t.Fatal("waiter without connectedAt must be signaled to query by itself")
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

func TestNotificationHubBatchErrorFallsBack(t *testing.T) {
	source := &fakeBatchSource{windows: []string{"w1"}, err: errors.New("db down")}
	hub := newNotificationHub(source)
	ready := hub.subscribe("u1", "asia")
	ready.setConnectedAt(time.Now())
	ctx := context.Background()

	hub.checkWindows(ctx)
	if !signaled(ready) {
		t.Fatal("on batch error, ready waiters must be signaled to query by themselves")
	}

	// 失敗の後は、一覧が同じでも次回の確認で必ずもう一度知らせる。
	source.err = nil
	hub.checkWindows(ctx)
	if len(source.batchCalls) != 2 {
		t.Fatalf("recovery after error must fetch again, got %d calls", len(source.batchCalls))
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
