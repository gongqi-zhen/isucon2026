package isureal

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// #85：投稿期間の開始で、前の投稿期間の行をロックしてから COMMIT までの間に、通知の一括挿入をしない。
// lock_contention_test.go と同じく、ISUREAL_TEST_DSN を指定したときだけ実行する。

// 投稿（FOR SHARE）が前の投稿期間の行を持っている間、投稿期間の開始は終了の UPDATE で待つ。
// そのときには、通知の一括挿入はもう終わっている（notifications に書き込みのロックを持っている）。
func TestPostingWindowSwitchInsertsNotificationsBeforeEndingWindow(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	if status, body := env.openWindow(admin); status != http.StatusCreated {
		t.Fatalf("open window: %d %v", status, body)
	}
	env.user("asia")

	// 投稿の関数と同じく、開いている投稿期間の行を FOR SHARE で持つ。
	ctx := context.Background()
	post, err := env.db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 途中で失敗しても、ロックを放してから投稿期間の開始のリクエストが終わるのを待つ（終わったテストの t を使わせない）。
	var wg sync.WaitGroup
	defer func() {
		post.Rollback()
		wg.Wait()
	}()
	var oldWindowID string
	if err := post.GetContext(ctx, &oldWindowID, `SELECT id FROM posting_windows WHERE region = 'asia' AND ended_at IS NULL FOR SHARE`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		status int
		body   map[string]any
	}
	done := make(chan result, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		status, body := env.openWindow(admin)
		done <- result{status, body}
	}()

	// 終了の UPDATE がロックを待つまで待つ。
	var waitingPID int
	deadline := time.Now().Add(10 * time.Second)
	for waitingPID == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the posting window switch did not wait for the post's FOR SHARE")
		}
		select {
		case r := <-done:
			t.Fatalf("the posting window switch finished while a post held FOR SHARE: %d %v", r.status, r.body)
		default:
		}
		env.db.Get(&waitingPID, `
   SELECT coalesce(max(pid), 0) FROM pg_stat_activity
   WHERE wait_event_type = 'Lock' AND query ~ '^\s*UPDATE posting_windows SET ended_at'
  `)
		if waitingPID == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	var holdsNotifications bool
	if err := env.db.Get(&holdsNotifications, `
  SELECT EXISTS(
    SELECT 1 FROM pg_locks
    WHERE pid = $1 AND relation = 'notifications'::regclass AND mode = 'RowExclusiveLock' AND granted
  )
 `, waitingPID); err != nil {
		t.Fatal(err)
	}
	if !holdsNotifications {
		t.Error("notifications were not inserted before ending the previous posting window")
	}

	if err := post.Rollback(); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the posting window switch did not finish after the post released FOR SHARE")
	}
	if r.status != http.StatusCreated {
		t.Fatalf("switch window: %d %v", r.status, r.body)
	}
	assertPostingWindowSwitch(t, env, oldWindowID, r.body)
}

// 投稿期間の開始の応答と、前の投稿期間の終了、通知の時刻と件数が、今まで通りであること。
func TestPostingWindowSwitchResponseAndTimestamps(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	if status, body := env.openWindow(admin); status != http.StatusCreated {
		t.Fatalf("open window: %d %v", status, body)
	}
	env.user("asia")
	var oldWindowID string
	if err := env.db.Get(&oldWindowID, `SELECT id FROM posting_windows WHERE region = 'asia' AND ended_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	status, body := env.openWindow(admin)
	if status != http.StatusCreated {
		t.Fatalf("switch window: %d %v", status, body)
	}
	assertPostingWindowSwitch(t, env, oldWindowID, body)

	// 同じ日付をもう一度開くと 409。
	status, conflict := env.do("POST", "/api/admin/posting-window", admin, map[string]string{"region": "asia", "localDate": body["localDate"].(string)}, nil)
	if status != http.StatusConflict || conflict["error"] != "posting_window_conflict" {
		t.Fatalf("same date: %d %v", status, conflict)
	}
	var open int
	env.db.Get(&open, `SELECT count(*) FROM posting_windows WHERE region = 'asia' AND ended_at IS NULL`)
	if open != 1 {
		t.Fatalf("%d open posting windows after the conflict", open)
	}
}

// assertPostingWindowSwitch は、新しい投稿期間の応答と DB の行が一致し、
// 前の投稿期間の ended_at、新しい投稿期間の starts_at、通知の created_at が同じ時刻で、
// 通知がそのリージョンの active な利用者に1件ずつあることを確かめる。
func assertPostingWindowSwitch(t *testing.T, env *lockTestEnv, oldWindowID string, body map[string]any) {
	t.Helper()
	windowID, _ := body["id"].(string)
	var window struct {
		PostingWindow
		PreviousEndedAt *time.Time `db:"previous_ended_at"`
	}
	if err := env.db.Get(&window, `
  SELECT w.id, w.region, w.local_date, w.starts_at, w.on_time_deadline, w.ended_at, old.ended_at AS previous_ended_at
  FROM posting_windows w, posting_windows old
  WHERE w.id = $1 AND old.id = $2
 `, windowID, oldWindowID); err != nil {
		t.Fatalf("new window %q: %v", windowID, err)
	}
	if window.EndedAt != nil {
		t.Errorf("new window has ended_at %v", window.EndedAt)
	}
	if window.PreviousEndedAt == nil || !window.PreviousEndedAt.Equal(window.StartsAt) {
		t.Errorf("previous ended_at %v, new starts_at %v", window.PreviousEndedAt, window.StartsAt)
	}
	if !window.OnTimeDeadline.Equal(window.StartsAt.Add(24 * time.Hour)) {
		t.Errorf("on_time_deadline %v, starts_at %v", window.OnTimeDeadline, window.StartsAt)
	}
	for key, want := range map[string]string{"id": window.ID, "region": "asia", "localDate": window.LocalDate.Format(time.DateOnly)} {
		if got, _ := body[key].(string); got != want {
			t.Errorf("response %s = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]time.Time{"startsAt": window.StartsAt, "onTimeDeadline": window.OnTimeDeadline} {
		got, _ := body[key].(string)
		if parsed, err := time.Parse(time.RFC3339Nano, got); err != nil || !parsed.Equal(want) {
			t.Errorf("response %s = %q, want %v", key, got, want)
		}
	}
	if len(body) != 5 {
		t.Errorf("response has %d fields: %v", len(body), body)
	}

	var notifications struct {
		Count      int `db:"count"`
		OtherTimes int `db:"other_times"`
		Missing    int `db:"missing"`
	}
	if err := env.db.Get(&notifications, `
  SELECT count(*) AS count,
         count(*) FILTER (WHERE n.created_at <> $2) AS other_times,
         (SELECT count(*) FROM users u WHERE u.region = 'asia' AND u.status = 'active'
            AND NOT EXISTS(SELECT 1 FROM notifications m WHERE m.posting_window_id = $1 AND m.user_id = u.id)) AS missing
  FROM notifications n WHERE n.posting_window_id = $1
 `, windowID, window.StartsAt); err != nil {
		t.Fatal(err)
	}
	if notifications.Count == 0 || notifications.OtherTimes != 0 || notifications.Missing != 0 {
		t.Errorf("notifications: %+v", notifications)
	}
}
