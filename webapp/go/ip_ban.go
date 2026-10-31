package isureal

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// BAN の一覧は、プロセスごとに1つの goroutine が ipBanReloadInterval ごとに読み直す。
// リクエストは読み込み済みの一覧を見るだけで、DB を待たない（europe と america では1往復が約100ms）。
//
// 一覧が ipBanMaxStaleness より古い場合（起動直後、読み直しの失敗や遅延が続いた場合）は、
// 以前と同じくリクエストの処理の中で読み直す。このとき、古い一覧のままで通すリクエストはない。
// 読み直しに失敗したら、そのリクエストはエラー（500）を返す。
const (
	ipBanReloadInterval = 200 * time.Millisecond
	// 読み直しが1回失敗しても、これを過ぎた一覧は使わない（以前の最大の古さ＝間隔＋1往復と同程度に抑える）。
	ipBanMaxStaleness = 500 * time.Millisecond
	ipBanRetryDelay   = 50 * time.Millisecond
	ipBanQueryTimeout = 2 * time.Second
)

type ipBanLoader func(ctx context.Context) (map[string]time.Time, error)

type IPBanList struct {
	load      ipBanLoader
	interval  time.Duration
	maxStale  time.Duration
	retry     time.Duration
	reloading sync.Mutex
	snapshot  atomic.Pointer[ipBanSnapshot]
	startOnce sync.Once
}

type ipBanSnapshot struct {
	// queriedAt は、この一覧を読んだクエリを送る直前の時刻。一覧の古さはこの時刻から数える。
	queriedAt time.Time
	bans      map[string]time.Time
}

type ipBan struct {
	IP          string    `db:"ip"`
	BannedUntil time.Time `db:"banned_until"`
}

func NewIPBanList(db *sqlx.DB) *IPBanList {
	return newIPBanList(func(ctx context.Context) (map[string]time.Time, error) {
		return loadIPBans(ctx, db)
	}, ipBanReloadInterval, ipBanMaxStaleness, ipBanRetryDelay)
}

func newIPBanList(load ipBanLoader, interval, maxStale, retry time.Duration) *IPBanList {
	return &IPBanList{load: load, interval: interval, maxStale: maxStale, retry: retry}
}

// Start は、バックグラウンドでの読み直しを始める。ctx が終わると止まる。2回目以降の呼び出しは何もしない。
func (l *IPBanList) Start(ctx context.Context) {
	l.startOnce.Do(func() {
		go l.run(ctx)
	})
}

func (l *IPBanList) run(ctx context.Context) {
	for {
		// 失敗したら前の一覧を残し、少し待ってすぐ読み直す。古くなりすぎた一覧は Banned が使わない。
		wait := l.interval
		if err := l.reload(ctx); err != nil {
			wait = l.retry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// reload は一覧を読み直す。並行して読み直さないよう、reloading を持って実行する。
func (l *IPBanList) reload(ctx context.Context) error {
	l.reloading.Lock()
	defer l.reloading.Unlock()
	return l.reloadLocked(ctx)
}

func (l *IPBanList) reloadLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ipBanQueryTimeout)
	defer cancel()
	queriedAt := time.Now()
	bans, err := l.load(ctx)
	if err != nil {
		return err
	}
	l.store(&ipBanSnapshot{queriedAt: queriedAt, bans: bans})
	return nil
}

// store は、今の一覧より新しいクエリの結果だけを保存する。
func (l *IPBanList) store(next *ipBanSnapshot) {
	for {
		current := l.snapshot.Load()
		if current != nil && !next.queriedAt.After(current.queriedAt) {
			return
		}
		if l.snapshot.CompareAndSwap(current, next) {
			return
		}
	}
}

func (l *IPBanList) fresh(snapshot *ipBanSnapshot) bool {
	return snapshot != nil && time.Since(snapshot.queriedAt) < l.maxStale
}

func (l *IPBanList) Banned(ctx context.Context, ip string) (bool, error) {
	snapshot := l.snapshot.Load()
	if !l.fresh(snapshot) {
		// 一覧が古すぎるときは、新しい一覧が手に入るまで通さない。
		// 先に読み直しを始めたリクエスト（またはバックグラウンド）が終わるのを待ち、まだ古ければ自分で読み直す。
		l.reloading.Lock()
		snapshot = l.snapshot.Load()
		if !l.fresh(snapshot) {
			if err := l.reloadLocked(ctx); err != nil {
				l.reloading.Unlock()
				return false, err
			}
			snapshot = l.snapshot.Load()
		}
		l.reloading.Unlock()
	}
	until, ok := snapshot.bans[ip]
	return ok && time.Now().Before(until), nil
}

func loadIPBans(ctx context.Context, db *sqlx.DB) (map[string]time.Time, error) {
	var rows []ipBan
	if err := db.SelectContext(ctx, &rows, `SELECT ip, banned_until FROM ip_bans WHERE banned_until > clock_timestamp()`); err != nil {
		return nil, err
	}
	bans := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		bans[row.IP] = row.BannedUntil
	}
	return bans, nil
}

func extractClientIP(req *http.Request) string {
	if ip := req.Header.Get("X-Client-IP"); ip != "" {
		return ip
	}
	return echo.ExtractIPDirect()(req)
}

func IPBanMiddleware(h *HandlerContext) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			banned, err := h.IPBans.Banned(c.Request().Context(), c.RealIP())
			if err != nil {
				return err
			}
			if banned {
				return NewAPIError(http.StatusForbidden, "ip_banned")
			}
			return next(c)
		}
	}
}
