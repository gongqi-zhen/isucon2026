package isureal

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"
)

// 無効化の一括検知（image_validity.go）の、時計とスナップショットを差し替えたテスト。

type imageTestClock struct{ now time.Time }

func (c *imageTestClock) Now() time.Time          { return c.now }
func (c *imageTestClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestTracker(clock *imageTestClock) *imageValidityTracker {
	tracker := newImageValidityTracker()
	tracker.now = clock.Now
	return tracker
}

var testPostMeta = imageValidityMeta{OwnerID: "owner", PostIDs: []string{"post"}}

func TestImageValidityNeedsBaselineAndFreshPoll(t *testing.T) {
	clock := &imageTestClock{now: time.Unix(1000, 0)}
	tracker := newTestTracker(clock)

	// 起点の読み込みより前に確かめた画像は覚えない。
	tracker.add("img", testPostMeta, clock.now)
	if tracker.valid("img") {
		t.Fatal("must not trust before the first poll")
	}
	sentAt := clock.now
	clock.Advance(100 * time.Millisecond)
	tracker.apply(imageInvalidationPoll{Epoch: "e1", XMin: "10"}, sentAt, clock.now)
	if tracker.valid("img") {
		t.Fatal("validation sent before the baseline must not be trusted")
	}
	// 起点より前に送った確認の結果は、後から届いても覚えない。
	tracker.add("img", testPostMeta, sentAt)
	if tracker.valid("img") {
		t.Fatal("validation sent before the baseline must not be added")
	}
	tracker.add("img", testPostMeta, clock.now)
	if !tracker.valid("img") {
		t.Fatal("validation after the baseline should be trusted")
	}
	// 読み込みが止まって鮮度が切れたら使わない。
	clock.Advance(imageInvalidationTrustWindow + time.Millisecond)
	if tracker.valid("img") {
		t.Fatal("must not trust when the last poll is older than the trust window")
	}
	// 次の読み込みが成功すれば、また使う（連鎖は xmin で続いている）。
	tracker.apply(imageInvalidationPoll{Epoch: "e1", XMin: "11"}, clock.now, clock.now)
	if !tracker.valid("img") {
		t.Fatal("should trust again after a fresh poll")
	}
}

func TestImageValidityInvalidationsForgetAndBlockStaleResults(t *testing.T) {
	cases := []struct {
		name string
		row  imageInvalidationRow
		meta imageValidityMeta
	}{
		{"image", imageInvalidationRow{ImageID: "img"}, testPostMeta},
		{"owner", imageInvalidationRow{UserID: "owner"}, testPostMeta},
		{"post", imageInvalidationRow{PostID: "post"}, testPostMeta},
		{"realmoji post author", imageInvalidationRow{UserID: "author"}, imageValidityMeta{OwnerID: "reactor", PostIDs: []string{"post"}, PostAuthorIDs: []string{"author"}}},
		{"realmoji post", imageInvalidationRow{PostID: "post"}, imageValidityMeta{OwnerID: "reactor", PostIDs: []string{"post"}, PostAuthorIDs: []string{"author"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &imageTestClock{now: time.Unix(1000, 0)}
			tracker := newTestTracker(clock)
			tracker.apply(imageInvalidationPoll{Epoch: "e1", XMin: "1"}, clock.now, clock.now)
			clock.Advance(time.Millisecond)
			validatedAt := clock.now
			tracker.add("img", tc.meta, validatedAt)
			tracker.add("other", imageValidityMeta{OwnerID: "someone", PostIDs: []string{"another"}}, validatedAt)
			if !tracker.valid("img") || !tracker.valid("other") {
				t.Fatal("should be trusted before invalidation")
			}
			// 確認のクエリを送った後に、無効化を反映した。
			clock.Advance(50 * time.Millisecond)
			tracker.apply(imageInvalidationPoll{Epoch: "e1", XMin: "2", Rows: []imageInvalidationRow{tc.row}}, clock.now, clock.now)
			if tracker.valid("img") {
				t.Fatal("invalidated image must be forgotten")
			}
			if !tracker.valid("other") {
				t.Fatal("unrelated image should stay")
			}
			// 無効化より前に送った確認の結果が遅れて届いても、覚えない。
			tracker.add("img", tc.meta, validatedAt)
			if tracker.valid("img") {
				t.Fatal("stale validation result must not be re-added")
			}
			// 無効化の後に送った確認（例えば初期化で戻った場合）は覚えてよい。
			clock.Advance(time.Millisecond)
			tracker.add("img", tc.meta, clock.now)
			if !tracker.valid("img") {
				t.Fatal("validation sent after the invalidation should be trusted")
			}
		})
	}
}

func TestImageValidityEpochChangeAndResetClear(t *testing.T) {
	clock := &imageTestClock{now: time.Unix(1000, 0)}
	tracker := newTestTracker(clock)
	tracker.apply(imageInvalidationPoll{Epoch: "e1", XMin: "1"}, clock.now, clock.now)
	clock.Advance(time.Millisecond)
	tracker.add("img", testPostMeta, clock.now)

	// 初期化で世代が変わったら、覚えている画像を捨てて、その読み込みを新しい起点にする。
	clock.Advance(time.Millisecond)
	beforeEpoch := clock.now
	clock.Advance(time.Millisecond)
	tracker.apply(imageInvalidationPoll{Epoch: "e2", XMin: "5"}, clock.now, clock.now)
	if tracker.valid("img") {
		t.Fatal("epoch change must clear entries")
	}
	tracker.add("img", testPostMeta, beforeEpoch)
	if tracker.valid("img") {
		t.Fatal("validation sent before the new baseline must not be added")
	}
	clock.Advance(time.Millisecond)
	tracker.add("img", testPostMeta, clock.now)
	if !tracker.valid("img") {
		t.Fatal("validation after the new baseline should be trusted")
	}

	// reset（初期化のリクエストを受けたプロセス）でも捨て、次の読み込みまで起点がない。
	tracker.reset()
	if tracker.valid("img") {
		t.Fatal("reset must clear entries")
	}
	tracker.add("img", testPostMeta, clock.now)
	if tracker.valid("img") {
		t.Fatal("must not trust before the next poll after reset")
	}
	clock.Advance(time.Millisecond)
	tracker.apply(imageInvalidationPoll{Epoch: "e2", XMin: "6"}, clock.now, clock.now)
	clock.Advance(time.Millisecond)
	tracker.add("img", testPostMeta, clock.now)
	if !tracker.valid("img") {
		t.Fatal("should trust after the next poll")
	}
}

type failingInvalidationSource struct{}

func (failingInvalidationSource) poll(context.Context, string) (imageInvalidationPoll, error) {
	return imageInvalidationPoll{}, errors.New("relation \"image_invalidation_epoch\" does not exist")
}

func TestImageValidityPollFailureKeepsState(t *testing.T) {
	clock := &imageTestClock{now: time.Unix(1000, 0)}
	tracker := newTestTracker(clock)
	tracker.apply(imageInvalidationPoll{Epoch: "e1", XMin: "7"}, clock.now, clock.now)
	if err := tracker.pollOnce(context.Background(), failingInvalidationSource{}); err == nil {
		t.Fatal("expected error")
	}
	if tracker.xmin != "7" || tracker.epoch != "e1" {
		t.Fatalf("failed poll must not change state: xmin=%q epoch=%q", tracker.xmin, tracker.epoch)
	}
}

// 削除のコミットの時刻 c に対して、スナップショットが c より後の最初の（成功した）読み込みだけが、その無効化を読む
// （DB の xid の連鎖が保つ最低限の性質。それより後の読み込みは、xmin が進むと同じ行を読まない。
// 連鎖そのものは image_invalidation_db_test.go で確かめる）。
// 読み込みの遅れと失敗、確認の結果の遅れを乱数で変えて、削除の応答の5秒後以降に、覚えている画像として返さないことを確かめる。
type simulatedPoll struct {
	sentAt, snapshotAt, receivedAt time.Time
	failed                         bool
}

func TestImageValidityNeverTrustedFiveSecondsAfterDeletion(t *testing.T) {
	rng := rand.New(rand.NewSource(64))
	for trial := range 2000 {
		start := time.Unix(1000, 0)
		clock := &imageTestClock{now: start}
		tracker := newTestTracker(clock)

		// 読み込みの予定：前回の結果を受け取ってから、500ms ごとの次の時刻に送る（1本の goroutine なので重ならない）。
		// 片道 0〜600ms、ときどき失敗が続く。
		var polls []simulatedPoll
		at := start
		failUntil := time.Time{}
		for at.Before(start.Add(30 * time.Second)) {
			if rng.Intn(40) == 0 {
				failUntil = at.Add(time.Duration(rng.Intn(8000)) * time.Millisecond)
			}
			d1 := time.Duration(rng.Intn(600)) * time.Millisecond
			d2 := time.Duration(rng.Intn(600)) * time.Millisecond
			poll := simulatedPoll{sentAt: at, snapshotAt: at.Add(d1), receivedAt: at.Add(d1 + d2), failed: at.Before(failUntil)}
			polls = append(polls, poll)
			at = at.Add(imageInvalidationPollInterval)
			if at.Before(poll.receivedAt) {
				at = poll.receivedAt
			}
		}
		// 有効と確かめるクエリ：sentAt に送り、validationSnapshot で読み、結果は遅れて届くことがある。
		validationSent := start.Add(time.Duration(2000+rng.Intn(5000)) * time.Millisecond)
		validationSnapshot := validationSent.Add(time.Duration(rng.Intn(300)) * time.Millisecond)
		validationReceived := validationSnapshot.Add(time.Duration(rng.Intn(2000)) * time.Millisecond)
		// 削除はその確認のスナップショットより後にコミットし（そうでなければ確認は無効と返す）、その後に応答する。
		commitAt := validationSnapshot.Add(time.Duration(1+rng.Intn(3000)) * time.Millisecond)
		responseAt := commitAt.Add(time.Duration(rng.Intn(200)) * time.Millisecond)

		// 時刻の順に、読み込みの結果の反映、確認の結果の追加、取得を行う。
		type event struct {
			at   time.Time
			kind int // 0: 読み込みの反映、1: 確認の結果、2: 取得
			poll simulatedPoll
		}
		var events []event
		for _, p := range polls {
			if !p.failed {
				events = append(events, event{at: p.receivedAt, kind: 0, poll: p})
			}
		}
		events = append(events, event{at: validationReceived, kind: 1})
		for check := responseAt.Add(5 * time.Second); check.Before(start.Add(30 * time.Second)); check = check.Add(97 * time.Millisecond) {
			events = append(events, event{at: check, kind: 2})
		}
		// 単純な挿入ソート（件数は少ない）。同時刻は配列の順。
		for i := 1; i < len(events); i++ {
			for j := i; j > 0 && events[j].at.Before(events[j-1].at); j-- {
				events[j], events[j-1] = events[j-1], events[j]
			}
		}
		delivered := false
		for _, e := range events {
			clock.now = e.at
			switch e.kind {
			case 0:
				result := imageInvalidationPoll{Epoch: "e1", XMin: "1"}
				if !delivered && e.poll.snapshotAt.After(commitAt) {
					result.Rows = []imageInvalidationRow{{PostID: "post"}}
					delivered = true
				}
				tracker.apply(result, e.poll.sentAt, e.poll.receivedAt)
			case 1:
				tracker.add("img", testPostMeta, validationSent)
			case 2:
				if tracker.valid("img") {
					t.Fatalf("trial %d: image trusted at %v, %v after the deletion response", trial, e.at.Sub(start), e.at.Sub(responseAt))
				}
			}
		}
	}
}

// 読み込みが順調なら、削除されていない画像は DB を引かずに返せる（効果の確認）。
func TestImageValidityTrustedWhilePollsAreHealthy(t *testing.T) {
	clock := &imageTestClock{now: time.Unix(1000, 0)}
	tracker := newTestTracker(clock)
	tracker.apply(imageInvalidationPoll{Epoch: "e1", XMin: "1"}, clock.now, clock.now.Add(100*time.Millisecond))
	clock.Advance(200 * time.Millisecond)
	tracker.add("img", testPostMeta, clock.now)
	for range 100 {
		clock.Advance(imageInvalidationPollInterval)
		sentAt := clock.now
		clock.Advance(100 * time.Millisecond)
		tracker.apply(imageInvalidationPoll{Epoch: "e1", XMin: "2"}, sentAt, clock.now)
		if !tracker.valid("img") {
			t.Fatal("image should stay trusted while polls are healthy")
		}
	}
}
