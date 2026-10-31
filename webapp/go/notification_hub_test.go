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
