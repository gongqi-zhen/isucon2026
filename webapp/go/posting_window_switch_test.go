package isureal

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// #85：投稿期間の開始で、前の投稿期間の行をロックしてから確定までの間に、通知の一括挿入をしない。
// posting_window_open_db_test.go と同じく、ISUREAL_TEST_DSN を指定したときだけ実行する。

// 投稿（FOR SHARE）が前の投稿期間の行を持っている間、投稿期間の開始（isureal_open_posting_window）は終了の UPDATE で待つ。
// そのときには、通知の一括挿入はもう終わっている（notifications に書き込みのロックを持っている）。
// 待ったあとも、前の投稿期間の ended_at、新しい投稿期間の starts_at、通知の created_at は同じ時刻になる。
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

	// 投稿期間の開始の関数が、行のロックを待つまで待つ。関数の中で待つのは、前の投稿期間を終える UPDATE だけ。
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
   WHERE wait_event_type = 'Lock' AND query LIKE '%isureal_open_posting_window%'
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

	windowID, _ := r.body["id"].(string)
	var times struct {
		StartsAt        time.Time  `db:"starts_at"`
		PreviousEndedAt *time.Time `db:"previous_ended_at"`
		Notifications   int        `db:"notifications"`
		OtherTimes      int        `db:"other_times"`
	}
	if err := env.db.Get(&times, `
  SELECT w.starts_at, old.ended_at AS previous_ended_at,
         (SELECT count(*) FROM notifications n WHERE n.posting_window_id = w.id) AS notifications,
         (SELECT count(*) FROM notifications n WHERE n.posting_window_id = w.id AND n.created_at <> w.starts_at) AS other_times
  FROM posting_windows w, posting_windows old
  WHERE w.id = $1 AND old.id = $2
 `, windowID, oldWindowID); err != nil {
		t.Fatalf("new window %q: %v", windowID, err)
	}
	if times.PreviousEndedAt == nil || !times.PreviousEndedAt.Equal(times.StartsAt) {
		t.Errorf("previous ended_at %v, new starts_at %v", times.PreviousEndedAt, times.StartsAt)
	}
	if times.Notifications == 0 || times.OtherTimes != 0 {
		t.Errorf("notifications: %d, with another created_at: %d", times.Notifications, times.OtherTimes)
	}
}
