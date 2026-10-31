package isureal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeNotificationSource struct {
	mu         sync.Mutex
	windows    []string
	windowsErr error
	regions    map[string]string
	usersErr   error
	userCalls  [][]string
}

func (s *fakeNotificationSource) ActiveWindowIDs(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.windows...), s.windowsErr
}

func (s *fakeNotificationSource) ActiveUserRegions(_ context.Context, userIDs []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userCalls = append(s.userCalls, append([]string(nil), userIDs...))
	if s.usersErr != nil {
		return nil, s.usersErr
	}
	regions := map[string]string{}
	for _, id := range userIDs {
		if region, ok := s.regions[id]; ok {
			regions[id] = region
		}
	}
	return regions, nil
}

func signaled(w *notificationWaiter) bool {
	select {
	case <-w.changed:
		return true
	default:
		return false
	}
}

func isGone(w *notificationWaiter) bool {
	select {
	case <-w.gone:
		return true
	default:
		return false
	}
}

func TestNotificationHubSignalsOnWindowChange(t *testing.T) {
	source := &fakeNotificationSource{windows: []string{"w1"}}
	hub := newNotificationHub(source)
	waiter := hub.subscribe("u1", "asia")
	ctx := context.Background()

	hub.checkWindows(ctx)
	if !signaled(waiter) {
		t.Fatal("first check must signal waiters")
	}
	hub.checkWindows(ctx)
	if signaled(waiter) {
		t.Fatal("unchanged windows must not signal")
	}

	source.windows = []string{"w1", "w2"}
	hub.checkWindows(ctx)
	if !signaled(waiter) {
		t.Fatal("new window must signal")
	}

	source.windows = []string{"w2"}
	hub.checkWindows(ctx)
	if !signaled(waiter) {
		t.Fatal("ended window must signal")
	}

	source.windows = nil
	hub.checkWindows(ctx)
	if !signaled(waiter) {
		t.Fatal("all windows ended (e.g. initialize) must signal")
	}
	hub.checkWindows(ctx)
	if signaled(waiter) {
		t.Fatal("still no windows must not signal")
	}
}

func TestNotificationHubSignalsAfterError(t *testing.T) {
	source := &fakeNotificationSource{windows: []string{"w1"}}
	hub := newNotificationHub(source)
	waiter := hub.subscribe("u1", "asia")
	ctx := context.Background()

	hub.checkWindows(ctx)
	signaled(waiter)

	source.windowsErr = errors.New("db down")
	hub.checkWindows(ctx)
	if signaled(waiter) {
		t.Fatal("error must not signal")
	}

	source.windowsErr = nil
	hub.checkWindows(ctx)
	if !signaled(waiter) {
		t.Fatal("recovery after error must signal even if windows look unchanged")
	}
}

func TestNotificationHubSignalIsCoalesced(t *testing.T) {
	source := &fakeNotificationSource{windows: []string{"w1"}}
	hub := newNotificationHub(source)
	waiter := hub.subscribe("u1", "asia")
	ctx := context.Background()

	hub.checkWindows(ctx)
	source.windows = []string{"w2"}
	hub.checkWindows(ctx)
	if !signaled(waiter) {
		t.Fatal("expected signal")
	}
	if signaled(waiter) {
		t.Fatal("pending signals must be coalesced into one")
	}
}

func TestNotificationHubUnsubscribe(t *testing.T) {
	source := &fakeNotificationSource{windows: []string{"w1"}}
	hub := newNotificationHub(source)
	waiter := hub.subscribe("u1", "asia")
	hub.unsubscribe(waiter)

	hub.checkWindows(context.Background())
	if signaled(waiter) {
		t.Fatal("unsubscribed waiter must not be signaled")
	}
	hub.checkUsers(context.Background())
	if len(source.userCalls) != 0 {
		t.Fatal("no user check expected without waiters")
	}
}

func TestNotificationHubClosesGoneUsers(t *testing.T) {
	source := &fakeNotificationSource{regions: map[string]string{"active": "asia", "moved": "europe"}}
	hub := newNotificationHub(source)
	active := hub.subscribe("active", "asia")
	activeAgain := hub.subscribe("active", "asia")
	moved := hub.subscribe("moved", "asia")
	deleted := hub.subscribe("deleted", "asia")
	ctx := context.Background()

	hub.checkUsers(ctx)
	if isGone(active) || isGone(activeAgain) {
		t.Fatal("active user in same region must stay")
	}
	if !isGone(moved) {
		t.Fatal("user who changed region must be closed")
	}
	if !isGone(deleted) {
		t.Fatal("deleted user must be closed")
	}
	if got := source.userCalls[0]; len(got) != 3 {
		t.Fatalf("user ids must be deduplicated into one query, got %v", got)
	}

	// 2回目の判定でも二重 close で panic しないこと。
	hub.checkUsers(ctx)
}

func TestNotificationHubKeepsUsersOnError(t *testing.T) {
	source := &fakeNotificationSource{usersErr: errors.New("db down")}
	hub := newNotificationHub(source)
	waiter := hub.subscribe("u1", "asia")

	hub.checkUsers(context.Background())
	if isGone(waiter) {
		t.Fatal("db error must not close connections")
	}
}

func TestNotificationHubRun(t *testing.T) {
	source := &fakeNotificationSource{windows: []string{"w1"}, regions: map[string]string{"u1": "asia"}}
	hub := newNotificationHub(source)
	// 利用者の確認のポーリングは取りこぼしの保険として残している（#110）。ここではその経路を短い間隔で確かめる。
	hub.userCheckInterval = 200 * time.Millisecond
	waiter := hub.subscribe("u1", "asia")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.run(ctx)

	select {
	case <-waiter.changed:
	case <-time.After(time.Second):
		t.Fatal("initial signal not delivered")
	}

	source.mu.Lock()
	source.windows = []string{"w2"}
	source.mu.Unlock()
	start := time.Now()
	select {
	case <-waiter.changed:
	case <-time.After(time.Second):
		t.Fatal("window change not delivered")
	}
	if elapsed := time.Since(start); elapsed > 2*notificationWindowPollInterval+50*time.Millisecond {
		t.Fatalf("window change took too long: %v", elapsed)
	}

	source.mu.Lock()
	delete(source.regions, "u1")
	source.mu.Unlock()
	select {
	case <-waiter.gone:
	case <-time.After(2 * time.Second):
		t.Fatal("deleted user not closed")
	}
}

// #110：LISTEN で受け取った利用者の変更は、その利用者の接続だけを確かめて閉じる。ほかの利用者は DB に問い合わせない。
func TestNotificationHubChecksOnlyChangedUsers(t *testing.T) {
	source := &fakeNotificationSource{regions: map[string]string{"active": "asia", "moved": "europe", "other": "asia"}}
	hub := newNotificationHub(source)
	active := hub.subscribe("active", "asia")
	other := hub.subscribe("other", "asia")
	moved := hub.subscribe("moved", "asia")
	movedAgain := hub.subscribe("moved", "asia")
	deleted := hub.subscribe("deleted", "asia")
	ctx := context.Background()

	hub.checkUsersFor(ctx, []string{"moved", "deleted", "nobody-waiting"})
	if isGone(active) || isGone(other) {
		t.Fatal("users that did not change must not be closed")
	}
	if !isGone(moved) || !isGone(movedAgain) {
		t.Fatal("every connection of the user who changed region must be closed")
	}
	if !isGone(deleted) {
		t.Fatal("deleted user must be closed")
	}
	if len(source.userCalls) != 1 {
		t.Fatalf("want one query, got %d", len(source.userCalls))
	}
	if got := source.userCalls[0]; len(got) != 2 || got[0] != "deleted" || got[1] != "moved" {
		t.Fatalf("query must cover only the changed users with waiters, deduplicated and sorted, got %v", got)
	}

	// 待っている接続のない利用者だけなら、問い合わせない。
	hub.checkUsersFor(ctx, []string{"nobody-waiting"})
	if len(source.userCalls) != 1 {
		t.Fatal("no waiter for the changed user: no query expected")
	}
}

// #110：notifyUserChanged で渡した利用者は、ポーリングを待たずに確かめられる。たまった分はまとめて1クエリで引く。
func TestNotificationHubUserChangeIsCheckedWithoutPolling(t *testing.T) {
	source := &fakeNotificationSource{windows: []string{"w1"}, regions: map[string]string{"active": "asia"}}
	hub := newNotificationHub(source)
	hub.windowPollInterval = time.Hour
	hub.userCheckInterval = time.Hour
	active := hub.subscribe("active", "asia")
	deleted := hub.subscribe("deleted", "asia")
	moved := hub.subscribe("moved", "asia")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.run(ctx)

	hub.notifyUserChanged("deleted")
	hub.notifyUserChanged("moved")
	for _, waiter := range []*notificationWaiter{deleted, moved} {
		select {
		case <-waiter.gone:
		case <-time.After(time.Second):
			t.Fatal("changed user must be closed without waiting for the poll")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if isGone(active) {
		t.Fatal("unchanged user must stay")
	}
	source.mu.Lock()
	calls := len(source.userCalls)
	source.mu.Unlock()
	if calls < 1 || calls > 2 {
		t.Fatalf("want 1 or 2 queries (changes may be batched), got %d", calls)
	}
}

func TestNotificationHubUserCheckIntervalDefault(t *testing.T) {
	if notificationUserCheckInterval != 5*time.Second {
		t.Fatalf("notificationUserCheckInterval = %v, want 5s (LISTEN/NOTIFY is the primary path, #110)", notificationUserCheckInterval)
	}
	hub := newNotificationHub(&fakeNotificationSource{})
	if hub.userCheckInterval != notificationUserCheckInterval || cap(hub.userChanges) != userChangeQueueSize {
		t.Fatal("hub must start with the default interval and queue size")
	}
	// ためておける数を超えても notifyUserChanged は止まらない。
	for range userChangeQueueSize + 10 {
		hub.notifyUserChanged("u")
	}
}
