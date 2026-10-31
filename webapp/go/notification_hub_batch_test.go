package isureal

import (
	"context"
	"errors"
	"slices"
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

// #83：まとめ取りが失敗しても、各接続には知らせず（接続の数だけクエリを投げさせず）、次回の確認でまとめて引き直す。
func TestNotificationHubBatchErrorRetries(t *testing.T) {
	base := time.Now()
	notification := getNotificationsResponse{ID: "n1", PostingWindowID: "w1", Region: "asia", CreatedAt: base.Add(time.Second)}
	source := &fakeBatchSource{windows: []string{"w1"}, pending: map[string]getNotificationsResponse{"u1": notification}, err: errors.New("db down")}
	hub := newNotificationHub(source)
	ready := hub.subscribe("u1", "asia")
	ready.setConnectedAt(base)
	notReady := hub.subscribe("u2", "asia")
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		hub.checkWindows(ctx)
		if len(source.batchCalls) != i {
			t.Fatalf("check %d: failed batch must be retried on every check even if windows look unchanged, got %d calls", i, len(source.batchCalls))
		}
		if signaled(ready) {
			t.Fatalf("check %d: on batch error, ready waiters must not be signaled to query by themselves", i)
		}
		if _, ok := delivered(ready); ok {
			t.Fatalf("check %d: nothing must be delivered on batch error", i)
		}
	}
	// 接続した時刻が決まっていない接続は、まとめ取りに入れられないので、失敗しても今まで通り changed で知らせる。
	if !signaled(notReady) {
		t.Fatal("waiter without connectedAt must still be signaled to query by itself")
	}

	source.err = nil
	hub.checkWindows(ctx)
	if got, ok := delivered(ready); !ok || got != notification {
		t.Fatalf("recovery after error must deliver, got %v %v", got, ok)
	}
	hub.checkWindows(ctx)
	if len(source.batchCalls) != 3 {
		t.Fatalf("after recovery, unchanged windows must not fetch again, got %d calls", len(source.batchCalls))
	}
}

func TestNotificationHubFetcherUsesBatchSource(t *testing.T) {
	if hub := newNotificationHub(&fakeNotificationSource{}); hub.fetcher != nil {
		t.Fatal("fetcher must be set only for a batch source")
	}
	base := time.Now()
	notification := getNotificationsResponse{ID: "n1", PostingWindowID: "w1", Region: "asia", CreatedAt: base.Add(time.Second)}
	source := &fakeBatchSource{windows: []string{"w1"}, pending: map[string]getNotificationsResponse{"u1": notification}}
	hub := newNotificationHub(source)
	got, err := hub.fetcher.get(context.Background(), notificationKey{UserID: "u1", ConnectedAt: base})
	if err != nil || got == nil || *got != notification {
		t.Fatalf("got %v %v, want %v", got, err, notification)
	}
	got, err = hub.fetcher.get(context.Background(), notificationKey{UserID: "u1", ConnectedAt: base.Add(2 * time.Second)})
	if err != nil || got != nil {
		t.Fatalf("notification created before connecting must not be returned, got %v %v", got, err)
	}
}

// blockingFetch は、呼ばれるたびに keys を記録し、release を受け取るまで返さない fetch。
type blockingFetch struct {
	mu      sync.Mutex
	calls   [][]notificationKey
	started chan struct{}
	release chan error
	pending map[string]getNotificationsResponse
}

func newBlockingFetch(pending map[string]getNotificationsResponse) *blockingFetch {
	return &blockingFetch{started: make(chan struct{}, 10), release: make(chan error), pending: pending}
}

func (b *blockingFetch) fetch(_ context.Context, keys []notificationKey) ([]*getNotificationsResponse, error) {
	b.mu.Lock()
	b.calls = append(b.calls, append([]notificationKey(nil), keys...))
	b.mu.Unlock()
	b.started <- struct{}{}
	if err := <-b.release; err != nil {
		return nil, err
	}
	results := make([]*getNotificationsResponse, len(keys))
	for i, key := range keys {
		if n, ok := b.pending[key.UserID]; ok && n.CreatedAt.After(key.ConnectedAt) {
			results[i] = &n
		}
	}
	return results, nil
}

func waitQueued(t *testing.T, f *notificationFetcher, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		queued := len(f.queue)
		f.mu.Unlock()
		if queued == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued %d requests, want %d", queued, n)
		}
		time.Sleep(time.Millisecond)
	}
}

type fetchOutcome struct {
	notification *getNotificationsResponse
	err          error
}

func startGet(ctx context.Context, f *notificationFetcher, userID string, connectedAt time.Time) chan fetchOutcome {
	out := make(chan fetchOutcome, 1)
	go func() {
		n, err := f.get(ctx, notificationKey{UserID: userID, ConnectedAt: connectedAt})
		out <- fetchOutcome{n, err}
	}()
	return out
}

func receive(t *testing.T, out chan fetchOutcome) fetchOutcome {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(time.Second):
		t.Fatal("get did not return")
		return fetchOutcome{}
	}
}

// 1つだけなら待たずにすぐ引き、引いている間に来た問い合わせは、それが終わった後の1回にまとめる。
func TestNotificationFetcherCoalescesConcurrentRequests(t *testing.T) {
	base := time.Now()
	n1 := getNotificationsResponse{ID: "n1", PostingWindowID: "w1", Region: "asia", CreatedAt: base.Add(time.Second)}
	n2 := getNotificationsResponse{ID: "n2", PostingWindowID: "w1", Region: "asia", CreatedAt: base.Add(time.Second)}
	b := newBlockingFetch(map[string]getNotificationsResponse{"u1": n1, "u2": n2})
	f := newNotificationFetcher(b.fetch)
	ctx := context.Background()

	first := startGet(ctx, f, "u1", base)
	<-b.started // 1つ目は、ほかを待たずに引き始める。

	second := startGet(ctx, f, "u2", base)
	third := startGet(ctx, f, "u3", base)
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancelled := startGet(cancelledCtx, f, "u4", base)
	waitQueued(t, f, 3)
	cancel()
	if o := receive(t, cancelled); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("cancelled request must return its context error, got %v", o.err)
	}

	b.release <- nil
	if o := receive(t, first); o.err != nil || o.notification == nil || *o.notification != n1 {
		t.Fatalf("first: got %v %v, want %v", o.notification, o.err, n1)
	}
	<-b.started
	b.release <- nil
	if o := receive(t, second); o.err != nil || o.notification == nil || *o.notification != n2 {
		t.Fatalf("second: got %v %v, want %v", o.notification, o.err, n2)
	}
	if o := receive(t, third); o.err != nil || o.notification != nil {
		t.Fatalf("third: got %v %v, want none", o.notification, o.err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.calls) != 2 {
		t.Fatalf("want 2 fetches (the first alone, then the rest together), got %v", b.calls)
	}
	if len(b.calls[0]) != 1 || b.calls[0][0].UserID != "u1" {
		t.Fatalf("first fetch must be for u1 alone, got %v", b.calls[0])
	}
	// 閉じた接続の問い合わせは引かない。u2 と u3 は別々の goroutine から来るので、順番は決まらない。
	var secondUsers []string
	for _, key := range b.calls[1] {
		secondUsers = append(secondUsers, key.UserID)
	}
	slices.Sort(secondUsers)
	if !slices.Equal(secondUsers, []string{"u2", "u3"}) {
		t.Fatalf("second fetch must be for u2 and u3 together, got %v", b.calls[1])
	}
}

func TestNotificationFetcherError(t *testing.T) {
	base := time.Now()
	b := newBlockingFetch(map[string]getNotificationsResponse{})
	f := newNotificationFetcher(b.fetch)
	ctx := context.Background()

	first := startGet(ctx, f, "u1", base)
	<-b.started
	second := startGet(ctx, f, "u2", base)
	third := startGet(ctx, f, "u3", base)
	waitQueued(t, f, 2)
	b.release <- errors.New("db down")
	if o := receive(t, first); o.err == nil {
		t.Fatal("first: error must be returned")
	}
	<-b.started
	b.release <- errors.New("db down")
	for name, out := range map[string]chan fetchOutcome{"second": second, "third": third} {
		if o := receive(t, out); o.err == nil {
			t.Fatalf("%s: error must be returned to every request in the batch", name)
		}
	}

	// 失敗の後も、次の問い合わせを引けること。
	after := startGet(ctx, f, "u1", base)
	<-b.started
	b.release <- nil
	if o := receive(t, after); o.err != nil || o.notification != nil {
		t.Fatalf("after error: got %v %v, want none", o.notification, o.err)
	}
}

func TestNotificationFetcherResultCountMismatch(t *testing.T) {
	f := newNotificationFetcher(func(context.Context, []notificationKey) ([]*getNotificationsResponse, error) {
		return nil, nil
	})
	if _, err := f.get(context.Background(), notificationKey{UserID: "u1", ConnectedAt: time.Now()}); err == nil {
		t.Fatal("result count mismatch must be an error")
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
