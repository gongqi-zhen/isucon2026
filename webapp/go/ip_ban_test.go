package isureal

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIPBanDB は ip_bans の代わり。fail が立っている間は読み込みに失敗し、delay の間だけ読み込みを遅らせる。
type fakeIPBanDB struct {
	mu    sync.Mutex
	bans  map[string]time.Time
	fail  atomic.Bool
	delay atomic.Int64
	calls atomic.Int64
}

func newFakeIPBanDB() *fakeIPBanDB {
	return &fakeIPBanDB{bans: map[string]time.Time{}}
}

func (f *fakeIPBanDB) set(ip string, until time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bans[ip] = until
}

func (f *fakeIPBanDB) truncate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bans = map[string]time.Time{}
}

func (f *fakeIPBanDB) load(ctx context.Context) (map[string]time.Time, error) {
	f.calls.Add(1)
	if d := time.Duration(f.delay.Load()); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.fail.Load() {
		return nil, errors.New("db down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	bans := make(map[string]time.Time, len(f.bans))
	for ip, until := range f.bans {
		if until.After(now) {
			bans[ip] = until
		}
	}
	return bans, nil
}

const (
	testInterval = 20 * time.Millisecond
	testMaxStale = 50 * time.Millisecond
	testRetry    = 5 * time.Millisecond
)

func startTestIPBanList(t *testing.T, db *fakeIPBanDB) *IPBanList {
	t.Helper()
	l := newIPBanList(db.load, testInterval, testMaxStale, testRetry)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	l.Start(ctx)
	return l
}

// waitBanned は、Banned が want を返すまでの時間を測る。limit を過ぎたら失敗にする。
func waitBanned(t *testing.T, l *IPBanList, ip string, want bool, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		got, err := l.Banned(context.Background(), ip)
		if err != nil {
			t.Fatalf("Banned: %v", err)
		}
		if got == want {
			return time.Since(start)
		}
		if time.Since(start) > limit {
			t.Fatalf("Banned(%s) did not become %v within %v", ip, want, limit)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestIPBanBeforeFirstLoadDoesNotPass(t *testing.T) {
	db := newFakeIPBanDB()
	db.set("10.0.0.1", time.Now().Add(time.Minute))
	// バックグラウンドを始めずに呼んでも、BAN が効く（起動直後と同じ。リクエストの中で読み込む）。
	l := newIPBanList(db.load, testInterval, testMaxStale, testRetry)
	banned, err := l.Banned(context.Background(), "10.0.0.1")
	if err != nil || !banned {
		t.Fatalf("banned=%v err=%v, want true nil", banned, err)
	}
	banned, err = l.Banned(context.Background(), "10.0.0.2")
	if err != nil || banned {
		t.Fatalf("banned=%v err=%v, want false nil", banned, err)
	}
}

func TestIPBanFirstLoadFailureReturnsError(t *testing.T) {
	db := newFakeIPBanDB()
	db.fail.Store(true)
	l := newIPBanList(db.load, testInterval, testMaxStale, testRetry)
	if _, err := l.Banned(context.Background(), "10.0.0.1"); err == nil {
		t.Fatal("want error while the ban list cannot be loaded")
	}
}

func TestIPBanRequestsDoNotQueryWhileFresh(t *testing.T) {
	db := newFakeIPBanDB()
	l := startTestIPBanList(t, db)
	waitBanned(t, l, "10.0.0.1", false, time.Second)
	// 読み込みに時間がかかっても、一覧が新しい間はリクエストが待たない。
	// 本番の「間隔 200ms ＋ 1往復 約100ms < 古すぎる 500ms」と同じ関係にする（20ms ＋ 10ms < 50ms）。
	db.delay.Store(int64(10 * time.Millisecond))
	time.Sleep(testInterval)
	for i := 0; i < 200; i++ {
		start := time.Now()
		if _, err := l.Banned(context.Background(), "10.0.0.1"); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 10*time.Millisecond {
			t.Fatalf("request waited %v for a reload", d)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

func TestIPBanNewBanTakesEffectWithinInterval(t *testing.T) {
	db := newFakeIPBanDB()
	l := startTestIPBanList(t, db)
	waitBanned(t, l, "10.0.0.1", false, time.Second)
	for i := 0; i < 5; i++ {
		ip := "10.0.1." + string(rune('1'+i))
		db.set(ip, time.Now().Add(time.Minute))
		if d := waitBanned(t, l, ip, true, time.Second); d > 2*testInterval {
			t.Fatalf("ban took %v to take effect, want <= interval", d)
		}
	}
}

func TestIPBanTruncateClearsOldBans(t *testing.T) {
	db := newFakeIPBanDB()
	db.set("10.0.0.1", time.Now().Add(time.Minute))
	l := startTestIPBanList(t, db)
	waitBanned(t, l, "10.0.0.1", true, time.Second)
	// 初期化で ip_bans を空にした後、古い BAN は残らない。
	db.truncate()
	waitBanned(t, l, "10.0.0.1", false, testInterval+50*time.Millisecond)
}

func TestIPBanExpiresWithoutReload(t *testing.T) {
	db := newFakeIPBanDB()
	db.set("10.0.0.1", time.Now().Add(60*time.Millisecond))
	l := newIPBanList(db.load, time.Hour, time.Hour, testRetry)
	waitBanned(t, l, "10.0.0.1", true, time.Second)
	waitBanned(t, l, "10.0.0.1", false, time.Second)
}

func TestIPBanBackgroundFailureKeepsBansAndRecovers(t *testing.T) {
	db := newFakeIPBanDB()
	db.set("10.0.0.1", time.Now().Add(time.Minute))
	l := startTestIPBanList(t, db)
	waitBanned(t, l, "10.0.0.1", true, time.Second)

	db.fail.Store(true)
	// 一覧が古すぎない間は、前の一覧の BAN を使い続ける。
	time.Sleep(testMaxStale / 5)
	banned, err := l.Banned(context.Background(), "10.0.0.1")
	if err != nil || !banned {
		t.Fatalf("within max staleness: banned=%v err=%v, want true nil", banned, err)
	}
	// 古すぎる一覧は使わない。読み込めない間は、通さずにエラーを返す。
	time.Sleep(testMaxStale)
	if _, err := l.Banned(context.Background(), "10.0.0.2"); err == nil {
		t.Fatal("want error once the ban list is too stale")
	}

	// 失敗の間に追加された BAN も、復旧後すぐに効く。
	db.set("10.0.0.3", time.Now().Add(time.Minute))
	db.fail.Store(false)
	waitBanned(t, l, "10.0.0.3", true, time.Second)
	waitBanned(t, l, "10.0.0.2", false, time.Second)
}

func TestIPBanStaleRequestsLoadOnce(t *testing.T) {
	db := newFakeIPBanDB()
	db.set("10.0.0.1", time.Now().Add(time.Minute))
	db.delay.Store(int64(20 * time.Millisecond))
	// バックグラウンドを動かさない。古すぎる一覧を同時に見たリクエストが、それぞれ DB を引かないこと。
	l := newIPBanList(db.load, time.Hour, testMaxStale, testRetry)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			banned, err := l.Banned(context.Background(), "10.0.0.1")
			if err != nil || !banned {
				t.Errorf("banned=%v err=%v", banned, err)
			}
		}()
	}
	wg.Wait()
	if n := db.calls.Load(); n != 1 {
		t.Fatalf("loads = %d, want 1", n)
	}
}

func TestIPBanOlderResultDoesNotOverwriteNewer(t *testing.T) {
	l := newIPBanList(nil, time.Hour, time.Hour, testRetry)
	now := time.Now()
	newer := &ipBanSnapshot{queriedAt: now, bans: map[string]time.Time{"10.0.0.1": now.Add(time.Minute)}}
	older := &ipBanSnapshot{queriedAt: now.Add(-time.Second), bans: map[string]time.Time{}}
	l.store(newer)
	l.store(older)
	if l.snapshot.Load() != newer {
		t.Fatal("an older query result replaced a newer one")
	}
}

func TestIPBanConcurrentAccess(t *testing.T) {
	db := newFakeIPBanDB()
	l := startTestIPBanList(t, db)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := l.Banned(context.Background(), "10.0.0.1"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		db.set("10.0.0.1", time.Now().Add(time.Minute))
		time.Sleep(5 * time.Millisecond)
		db.truncate()
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
}
