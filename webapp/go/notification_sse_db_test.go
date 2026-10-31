package isureal

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// #60 の SSE 通知の配信を、実際の PostgreSQL と HTTP ハンドラで確かめる。
// ISUREAL_TEST_DSN に、001-schema.sql、migrate.sql、interaction_functions.sql を流した DB を指定したときだけ実行する。

type sseStream struct {
	events chan string
	closed chan struct{}
}

// openSSE は通知の SSE に接続し、応答のヘッダーを受け取るまで待つ。data 行の中身を events に流す。
func (env *lockTestEnv) openSSE(u lockTestUser) *sseStream {
	env.t.Helper()
	req, _ := http.NewRequest("GET", env.server.URL+"/api/notifications", nil)
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("X-Client-IP", fmt.Sprintf("10.60.%d.%d", time.Now().UnixNano()%250, time.Now().UnixNano()/7%250))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		env.t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		env.t.Fatalf("sse: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	stream := &sseStream{events: make(chan string, 4), closed: make(chan struct{})}
	go func() {
		defer close(stream.closed)
		scanner := bufio.NewScanner(res.Body)
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				stream.events <- data
			}
		}
	}()
	env.t.Cleanup(func() { res.Body.Close() })
	return stream
}

func (s *sseStream) next(t *testing.T, timeout time.Duration) (getNotificationsResponse, bool) {
	t.Helper()
	select {
	case data := <-s.events:
		var n struct {
			ID              string `json:"id"`
			PostingWindowID string `json:"postingWindowId"`
			Region          string `json:"region"`
			CreatedAt       string `json:"createdAt"`
		}
		if err := json.Unmarshal([]byte(data), &n); err != nil {
			t.Fatalf("bad event %q: %v", data, err)
		}
		createdAt, err := time.Parse(time.RFC3339Nano, n.CreatedAt)
		if err != nil {
			t.Fatalf("bad createdAt %q: %v", n.CreatedAt, err)
		}
		return getNotificationsResponse{ID: n.ID, PostingWindowID: n.PostingWindowID, Region: n.Region, CreatedAt: createdAt}, true
	case <-time.After(timeout):
		return getNotificationsResponse{}, false
	}
}

func (env *lockTestEnv) expectNotification(t *testing.T, u lockTestUser, got getNotificationsResponse, windowID string) {
	t.Helper()
	var want struct {
		ID        string    `db:"id"`
		CreatedAt time.Time `db:"created_at"`
	}
	if err := env.db.Get(&want, `SELECT id, created_at FROM notifications WHERE user_id = $1 AND posting_window_id = $2`, u.id, windowID); err != nil {
		t.Fatalf("notification row: %v", err)
	}
	if got.ID != want.ID || got.PostingWindowID != windowID || got.Region != "asia" || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("got %+v, want id=%s window=%s region=asia createdAt=%s", got, want.ID, windowID, want.CreatedAt)
	}
}

func TestNotificationSSEDelivery(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	users := []lockTestUser{env.user("asia"), env.user("asia"), env.user("asia")}
	streams := make([]*sseStream, len(users))
	for i, u := range users {
		streams[i] = env.openSSE(u)
	}

	start := time.Now()
	windowID := env.mustOpenWindow(admin)
	for i, u := range users {
		got, ok := streams[i].next(t, 3*time.Second)
		if !ok {
			t.Fatalf("user %d: no notification", i)
		}
		env.expectNotification(t, u, got, windowID)
		select {
		case <-streams[i].closed:
		case <-time.After(time.Second):
			t.Fatalf("user %d: stream must close after one notification", i)
		}
	}
	t.Logf("all %d notifications delivered %v after opening the window", len(users), time.Since(start))

	// 投稿期間の開始より後に接続した人には、その投稿期間の通知は届かない。次の投稿期間の通知は届く。
	late := env.user("asia")
	lateStream := env.openSSE(late)
	if got, ok := lateStream.next(t, 500*time.Millisecond); ok {
		t.Fatalf("notification created before connecting must not be delivered: %+v", got)
	}
	nextWindowID := env.mustOpenWindow(admin)
	got, ok := lateStream.next(t, 3*time.Second)
	if !ok {
		t.Fatal("late user: no notification for the next window")
	}
	env.expectNotification(t, late, got, nextWindowID)
}

// PendingNotifications が、接続ごとのクエリ（getNotificationsHandler のフォールバック）と同じ通知を返すこと。
// JSON にしたときの文字列（時刻の書式とタイムゾーン）まで同じであること。
func TestNotificationPendingMatchesPerConnectionQuery(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	source := dbNotificationSource{db: env.db}
	a, b, c := env.user("asia"), env.user("asia"), env.user("europe")
	clock := func() time.Time {
		var now time.Time
		if err := env.db.Get(&now, `SELECT clock_timestamp()`); err != nil {
			t.Fatal(err)
		}
		return now
	}
	beforeFirst := clock()
	env.mustOpenWindow(admin)
	between := clock()
	env.mustOpenWindow(admin)
	afterSecond := clock()
	keys := []notificationKey{
		{a.id, beforeFirst}, {a.id, between}, {a.id, afterSecond},
		{b.id, beforeFirst}, {b.id, beforeFirst}, // 同じ人の接続が2つ
		{c.id, beforeFirst}, // europe の人には asia の通知はない
		{"no-such-user", beforeFirst},
	}
	windows, pending, err := source.PendingNotifications(context.Background(), keys)
	if err != nil {
		t.Fatal(err)
	}
	wantWindows, err := source.ActiveWindowIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(windows, ",") != strings.Join(wantWindows, ",") {
		t.Fatalf("windows = %v, want %v", windows, wantWindows)
	}
	found := 0
	for i, key := range keys {
		var want getNotificationsResponse
		err := env.db.Get(&want, `
    SELECT n.id, n.posting_window_id, w.region, n.created_at
    FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
    WHERE n.user_id = $1 AND n.created_at > $2 AND w.ended_at IS NULL
    ORDER BY n.created_at, n.id LIMIT 1
   `, key.UserID, key.ConnectedAt)
		if errors.Is(err, sql.ErrNoRows) {
			if pending[i] != nil {
				t.Fatalf("key %d: got %+v, want none", i, *pending[i])
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if pending[i] == nil {
			t.Fatalf("key %d: got none, want %+v", i, want)
		}
		gotJSON, _ := json.Marshal(*pending[i])
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("key %d: got %s, want %s", i, gotJSON, wantJSON)
		}
		found++
	}
	// a は beforeFirst と between、b は2つとも見つかる（開いている2つ目の投稿期間の通知）。
	if found != 4 {
		t.Fatalf("found %d notifications, want 4", found)
	}
}

// countingBatchSource は、まとめ取りの呼び出しを calls に知らせる。最初の failures 回は失敗させる。
type countingBatchSource struct {
	notificationBatchSource
	calls    chan struct{}
	mu       sync.Mutex
	failures int
}

func newCountingBatchSource(source notificationBatchSource, failures int) *countingBatchSource {
	return &countingBatchSource{notificationBatchSource: source, calls: make(chan struct{}, 64), failures: failures}
}

func (s *countingBatchSource) PendingNotifications(ctx context.Context, keys []notificationKey) ([]string, []*getNotificationsResponse, error) {
	select {
	case s.calls <- struct{}{}:
	default:
	}
	s.mu.Lock()
	fail := s.failures > 0
	if fail {
		s.failures--
	}
	s.mu.Unlock()
	if fail {
		return nil, nil, errors.New("injected batch failure")
	}
	return s.notificationBatchSource.PendingNotifications(ctx, keys)
}

func (s *countingBatchSource) waitCall(t *testing.T, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-s.calls:
	case <-time.After(timeout):
		t.Fatal(what)
	}
}

func (s *countingBatchSource) drain() {
	for {
		select {
		case <-s.calls:
		default:
			return
		}
	}
}

func (env *lockTestEnv) clock() time.Time {
	env.t.Helper()
	var now time.Time
	if err := env.db.Get(&now, `SELECT clock_timestamp()`); err != nil {
		env.t.Fatal(err)
	}
	return now
}

// ポーリングを実質止めても、LISTEN で投稿期間の開始を受け取って通知を渡すこと。接続が切れてもつなぎ直すこと。
func TestNotificationListenerWakesHub(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	source := newCountingBatchSource(dbNotificationSource{db: env.db}, 0)
	hub := newNotificationHub(source)
	hub.windowPollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.run(ctx)

	// LISTEN を始めるとハブが起こされ、まとめ取りをする。それで LISTEN の開始を知る。
	go hub.listen(ctx, dsn)
	source.waitCall(t, 5*time.Second, "listener did not start")

	u := env.user("asia")
	var connectedAt time.Time
	if err := env.db.Get(&connectedAt, `SELECT clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	waiter := hub.subscribe(u.id, "asia")
	hub.setConnectedAt(waiter, connectedAt)
	windowID := env.mustOpenWindow(admin)
	select {
	case got := <-waiter.delivered:
		env.expectNotification(t, u, got, windowID)
	case <-time.After(2 * time.Second):
		t.Fatal("LISTEN did not deliver the notification")
	}

	// LISTEN している接続を DB 側から切っても、つなぎ直して届けること。
	// 投稿期間を開いたときの知らせが残っているので、先に捨てる。
	source.drain()
	if _, err := env.db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query = $1`, "LISTEN "+postingWindowNotifyChannel); err != nil {
		t.Fatal(err)
	}
	source.waitCall(t, 5*time.Second, "listener did not reconnect")
	waiter2 := hub.subscribe(u.id, "asia")
	hub.setConnectedAt(waiter2, connectedAt)
	windowID = env.mustOpenWindow(admin)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-waiter2.delivered:
			if got.PostingWindowID != windowID {
				continue // つなぎ直したときの確認で、前の投稿期間の通知が先に届くことがある
			}
			env.expectNotification(t, u, got, windowID)
			return
		case <-deadline:
			t.Fatal("LISTEN did not deliver after reconnecting")
		}
	}
}

// 投稿期間の開始は、COMMIT したときだけ pg_notify が届くこと（409 でロールバックしたときは届かない）。
func TestPostingWindowOpenSendsNotifyOnCommit(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("ISUREAL_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "LISTEN "+postingWindowNotifyChannel); err != nil {
		t.Fatal(err)
	}
	wait := func(timeout time.Duration) (string, bool) {
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		n, err := conn.WaitForNotification(waitCtx)
		if err != nil {
			return "", false
		}
		return n.Payload, true
	}

	env.mustOpenWindow(admin)
	if payload, ok := wait(2 * time.Second); !ok || payload != "asia" {
		t.Fatalf("expected notify with payload asia, got %q %v", payload, ok)
	}

	// すでにある日付で開くと 409 になり、通知は届かない。
	status, body := env.do("POST", "/api/admin/posting-window", admin, map[string]string{"region": "asia", "localDate": "2100-01-01"}, nil)
	if status != http.StatusConflict {
		t.Fatalf("conflict: %d %v", status, body)
	}
	if payload, ok := wait(300 * time.Millisecond); ok {
		t.Fatalf("rolled back open must not notify, got %q", payload)
	}
}

// 接続した時刻が決まる前に投稿期間が開き、まとめ取りがあっても、時刻が決まった後のまとめ取りで届くこと（#83）。
// その接続は自分で通知を引かない（changed で知らされない）。
func TestNotificationLateConnectedAtDeliveredByBatch(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	source := newCountingBatchSource(dbNotificationSource{db: env.db}, 0)
	hub := newNotificationHub(source)
	hub.windowPollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.run(ctx)

	u := env.user("asia")
	// getNotificationsHandler と同じ順：購読してから、接続した時刻を読む。
	waiter := hub.subscribe(u.id, "asia")
	connectedAt := env.clock()
	// 時刻を読んでから、それをハブに渡すまでの間に投稿期間が開き、ハブがまとめ取りをする。
	windowID := env.mustOpenWindow(admin)
	hub.notifyWindowChanged()
	source.waitCall(t, 2*time.Second, "hub did not run the batch")
	if _, ok := delivered(waiter); ok {
		t.Fatal("waiter without connectedAt must not get a notification yet")
	}

	hub.setConnectedAt(waiter, connectedAt)
	select {
	case got := <-waiter.delivered:
		env.expectNotification(t, u, got, windowID)
	case <-time.After(2 * time.Second):
		t.Fatal("notification was not delivered after connectedAt was set")
	}
	if signaled(waiter) {
		t.Fatal("waiter must not be told to query by itself")
	}
}

// まとめ取りが続けて失敗しても、上限の回数に届くまでは各接続に引かせず、やり直したまとめ取りで全員に届くこと（#83）。
func TestNotificationBatchFailureRetriesWithoutPerConnectionQueries(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	source := newCountingBatchSource(dbNotificationSource{db: env.db}, notificationBatchMaxFailures-1)
	hub := newNotificationHub(source)
	hub.windowPollInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	users := []lockTestUser{env.user("asia"), env.user("asia"), env.user("asia")}
	waiters := make([]*notificationWaiter, len(users))
	connectedAt := env.clock()
	for i, u := range users {
		waiters[i] = hub.subscribe(u.id, "asia")
		hub.setConnectedAt(waiters[i], connectedAt)
	}
	windowID := env.mustOpenWindow(admin)
	go hub.run(ctx)

	for i, u := range users {
		select {
		case got := <-waiters[i].delivered:
			env.expectNotification(t, u, got, windowID)
		case <-time.After(3 * time.Second):
			t.Fatalf("user %d: notification not delivered after batch retries", i)
		}
		if signaled(waiters[i]) {
			t.Fatalf("user %d: must not be told to query by itself before %d failures", i, notificationBatchMaxFailures)
		}
	}
}
