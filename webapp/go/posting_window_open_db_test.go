package isureal

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// 投稿期間の開始を DB の関数1回にした（#72）あとも、変更前と同じ結果になることを確かめる。
// ISUREAL_TEST_DSN に、001-schema.sql、migrate.sql、interaction_functions.sql、posting_window_functions.sql を流した DB を指定したときだけ実行する。

// nextLockTestDate は、openWindow と同じく、これまでより後の asia の日付を返す。
func (env *lockTestEnv) nextLockTestDate() string {
	lockTestDateMu.Lock()
	defer lockTestDateMu.Unlock()
	var latest time.Time
	env.db.Get(&latest, `SELECT coalesce(max(local_date), '2200-01-01') FROM posting_windows WHERE region = 'asia'`)
	if latest.After(lockTestDate) {
		lockTestDate = latest
	}
	lockTestDate = lockTestDate.AddDate(0, 0, 1)
	return lockTestDate.Format(time.DateOnly)
}

func TestPostingWindowOpenBehavior(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	if status, body := env.openWindow(admin); status != http.StatusCreated {
		t.Fatalf("open first window: %d %v", status, body)
	}
	var previousID string
	if err := env.db.Get(&previousID, `SELECT id FROM posting_windows WHERE region = 'asia' AND ended_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	env.user("asia")
	env.user("europe")
	inactive := env.user("asia")
	env.db.MustExec(`UPDATE users SET status = 'deleted' WHERE id = $1`, inactive.id)

	date := env.nextLockTestDate()
	status, body := env.do("POST", "/api/admin/posting-window", admin, map[string]any{"region": "asia", "localDate": date, "onTimeSeconds": 600}, nil)
	if status != http.StatusCreated {
		t.Fatalf("open: %d %v", status, body)
	}
	windowID := body["id"].(string)
	if body["region"] != "asia" || body["localDate"] != date {
		t.Fatalf("unexpected response: %v", body)
	}
	startsAt, err := time.Parse(time.RFC3339Nano, body["startsAt"].(string))
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, body["onTimeDeadline"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if got := deadline.Sub(startsAt); got != 600*time.Second {
		t.Fatalf("onTimeDeadline - startsAt = %v, want 10m", got)
	}

	// 応答は DB に入った行と同じで、前の投稿期間は新しい投稿期間の開始時刻で終わる。
	var stored PostingWindow
	if err := env.db.Get(&stored, `SELECT id, region, local_date, starts_at, on_time_deadline, ended_at FROM posting_windows WHERE id = $1`, windowID); err != nil {
		t.Fatal(err)
	}
	if !stored.StartsAt.Equal(startsAt) || !stored.OnTimeDeadline.Equal(deadline) || stored.EndedAt != nil || stored.LocalDate.Format(time.DateOnly) != date {
		t.Fatalf("stored window differs from response: %+v %v", stored, body)
	}
	var previousEndedAt *time.Time
	env.db.Get(&previousEndedAt, `SELECT ended_at FROM posting_windows WHERE id = $1`, previousID)
	if previousEndedAt == nil || !previousEndedAt.Equal(startsAt) {
		t.Fatalf("previous window ended_at = %v, want %v", previousEndedAt, startsAt)
	}
	var open int
	env.db.Get(&open, `SELECT count(*) FROM posting_windows WHERE region = 'asia' AND ended_at IS NULL`)
	if open != 1 {
		t.Fatalf("%d open windows in asia", open)
	}

	// 通知は、そのリージョンの active な利用者全員に1件ずつ、開始時刻で入る。ID の先頭48ビットは開始時刻（ミリ秒）。
	var want, got, wrongTime, wrongPrefix int
	env.db.Get(&want, `SELECT count(*) FROM users WHERE region = 'asia' AND status = 'active'`)
	env.db.Get(&got, `SELECT count(*) FROM notifications n JOIN users u ON u.id = n.user_id WHERE n.posting_window_id = $1 AND u.region = 'asia' AND u.status = 'active'`, windowID)
	if want == 0 || got != want {
		t.Fatalf("notifications for active asia users: got %d, want %d", got, want)
	}
	var total int
	env.db.Get(&total, `SELECT count(*) FROM notifications WHERE posting_window_id = $1`, windowID)
	if total != want {
		t.Fatalf("notifications for others: %d", total-want)
	}
	env.db.Get(&wrongTime, `SELECT count(*) FROM notifications WHERE posting_window_id = $1 AND created_at <> $2`, windowID, startsAt)
	prefix := strings.ReplaceAll(windowIDPrefix(startsAt), "-", "")
	env.db.Get(&wrongPrefix, `SELECT count(*) FROM notifications WHERE posting_window_id = $1 AND left(replace(id, '-', ''), 12) <> $2`, windowID, prefix)
	if wrongTime != 0 || wrongPrefix != 0 {
		t.Fatalf("notifications with wrong created_at: %d, wrong id prefix: %d", wrongTime, wrongPrefix)
	}

	// 同じ日付と前の日付は 409 で、何も変わらない。
	for _, d := range []string{date, "2000-01-01"} {
		status, body := env.do("POST", "/api/admin/posting-window", admin, map[string]any{"region": "asia", "localDate": d}, nil)
		if status != http.StatusConflict || body["error"] != "posting_window_conflict" {
			t.Fatalf("open %s: %d %v", d, status, body)
		}
	}
	var current string
	env.db.Get(&current, `SELECT id FROM posting_windows WHERE region = 'asia' AND ended_at IS NULL`)
	if current != windowID {
		t.Fatalf("conflict changed the open window: %s", current)
	}
}

// 同じリージョンで同時に同じ日付を開くと、1件だけ 201 になり、残りは 409 になる。開いている投稿期間は1つだけ。
func TestPostingWindowOpenConcurrentSameDate(t *testing.T) {
	env := newLockTestEnv(t)
	admin := env.adminToken()
	date := env.nextLockTestDate()
	statuses := make([]int, 8)
	parallel(len(statuses), func(i int) {
		statuses[i], _ = env.do("POST", "/api/admin/posting-window", admin, map[string]any{"region": "asia", "localDate": date}, nil)
	})
	created := 0
	for i, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Errorf("request %d: %d", i, status)
		}
	}
	if created != 1 {
		t.Fatalf("%d requests created a window", created)
	}
	var open, sameDate int
	env.db.Get(&open, `SELECT count(*) FROM posting_windows WHERE region = 'asia' AND ended_at IS NULL`)
	env.db.Get(&sameDate, `SELECT count(*) FROM posting_windows WHERE region = 'asia' AND local_date = $1`, date)
	if open != 1 || sameDate != 1 {
		t.Fatalf("open windows: %d, windows on %s: %d", open, date, sameDate)
	}
}

// windowIDPrefix は、t のミリ秒を UUID v7 の先頭48ビットにした16進12桁を返す。
func windowIDPrefix(t time.Time) string {
	const hex = "0123456789abcdef"
	ms := uint64(t.UnixMilli())
	b := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		b[i] = hex[ms&0xf]
		ms >>= 4
	}
	return string(b)
}
