package isureal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jmoiron/sqlx"
)

// 投稿期間の開始は別リージョンのサーバに届くことがあるため、変化は DB で検知する。
// 接続ごとに確認せず、プロセスで1本の goroutine が、開いている投稿期間と待機中ユーザーの状態をまとめて確認する（#11）。
//
// 検知と配信を速くするため（#60）：
//   - 投稿期間の開始は pg_notify で知らせ、各プロセスは専用の接続1本で LISTEN する。届いたらすぐに確認する。
//     ポーリングは、LISTEN の接続が切れているときなどの取りこぼしの保険として残す。
//   - 変化を見つけたら、待っている全員の通知を1クエリで引き、各接続に渡す。
//     以前は各接続がそれぞれ1クエリずつ引いていたので、投稿期間の開始の直後に DB の接続プールを取り合っていた。
//   - まとめて引けなかった接続（接続した時刻がまだ決まっていなかった接続）が自分で引くときも、
//     同時に引くものどうしを1クエリにまとめる（notificationFetcher、#83）。
//
// 利用者の退会とリージョンの変更（#110）：以前は notificationUserCheckInterval（500ms）ごとに、待機中の全接続の利用者を
// users から引き直していた。接続が 2,000 あれば毎秒 4,000 件の索引の読み取りになる。
// 今は users の status と region の変更（と DELETE）を、migrate.sql のトリガが pg_notify で知らせる。
// LISTEN する接続は投稿期間と同じ1本で、届いた利用者の接続だけを、以前と同じ判定（ActiveUserRegions）で確かめて閉じる。
// ポーリングは、LISTEN の接続が切れている間と、トリガが動かない初期化（reset.sql）の取りこぼしの保険として、間隔を伸ばして残す。
const (
	notificationWindowPollInterval = 100 * time.Millisecond
	notificationUserCheckInterval  = 5 * time.Second
	// 投稿期間の開始を知らせる pg_notify のチャンネル。admin_posting_window_api.go が送る。
	postingWindowNotifyChannel = "isureal_posting_windows"
	// 利用者の退会・リージョンの変更・削除を知らせる pg_notify のチャンネル。migrate.sql のトリガ isureal_notify_user_change が送る。
	// ペイロードは利用者の id。
	userChangeNotifyChannel = "isureal_users"
	// LISTEN の接続が切れたときに、つなぎ直すまで待つ時間。
	postingWindowListenRetry = time.Second
	// LISTEN で届いた利用者の変更を、確かめる goroutine に渡すまでためておける数。あふれた分はポーリングが拾う。
	userChangeQueueSize = 1024
)

// notificationListenStatement は、LISTEN する接続で実行する文。2つのチャンネルを1文で LISTEN する。
const notificationListenStatement = "LISTEN " + postingWindowNotifyChannel + "; LISTEN " + userChangeNotifyChannel

type notificationSource interface {
	// 開いている投稿期間のIDを返す。
	ActiveWindowIDs(ctx context.Context) ([]string, error)
	// 指定ユーザーのうち active なユーザーのリージョンを返す。
	ActiveUserRegions(ctx context.Context, userIDs []string) (map[string]string, error)
}

// notificationBatchSource を実装する source では、ハブが待機中の全員の通知をまとめて引いて渡す。
// 実装しない source では、以前と同じく待機中の接続に変化を知らせ、各接続が自分で通知を引く。
type notificationBatchSource interface {
	notificationSource
	// 開いている投稿期間のIDと、keys のそれぞれについて getNotificationsHandler が1件目に返す通知を、
	// 同じスナップショットで返す。通知がないキーは nil。
	PendingNotifications(ctx context.Context, keys []notificationKey) ([]string, []*getNotificationsResponse, error)
}

// notificationKey は、待機中の1接続が受け取れる通知の条件（そのユーザーの、接続した時刻より後に作られた通知）。
type notificationKey struct {
	UserID      string
	ConnectedAt time.Time
}

type dbNotificationSource struct {
	db *sqlx.DB
}

func (s dbNotificationSource) ActiveWindowIDs(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.db.SelectContext(ctx, &ids, `SELECT id FROM posting_windows WHERE ended_at IS NULL ORDER BY id`)
	return ids, err
}

func (s dbNotificationSource) ActiveUserRegions(ctx context.Context, userIDs []string) (map[string]string, error) {
	var rows []struct {
		ID     string `db:"id"`
		Region string `db:"region"`
	}
	if err := s.db.SelectContext(ctx, &rows, `SELECT id, region FROM users WHERE id = ANY($1) AND status = 'active'`, userIDs); err != nil {
		return nil, err
	}
	regions := make(map[string]string, len(rows))
	for _, row := range rows {
		regions[row.ID] = row.Region
	}
	return regions, nil
}

// pendingNotificationJSON は PendingNotifications の1件。
// created_at は、pgx で読んだ場合と同じ値（マイクロ秒、time.Local）に戻すため、エポックからのマイクロ秒で受け取る。
type pendingNotificationJSON struct {
	Index           int    `json:"i"`
	ID              string `json:"id"`
	PostingWindowID string `json:"posting_window_id"`
	Region          string `json:"region"`
	CreatedAtMicros int64  `json:"created_at_us"`
}

// PendingNotifications の LATERAL の中は、getNotificationsHandler が接続ごとに引くクエリと同じ条件と並び順にする。
// 開いている投稿期間も同じ文で読むので、通知と投稿期間の一覧は同じスナップショットになる。
func (s dbNotificationSource) PendingNotifications(ctx context.Context, keys []notificationKey) ([]string, []*getNotificationsResponse, error) {
	userIDs := make([]string, len(keys))
	connectedAts := make([]time.Time, len(keys))
	for i, key := range keys {
		userIDs[i], connectedAts[i] = key.UserID, key.ConnectedAt
	}
	var row struct {
		Windows       string `db:"windows"`
		Notifications string `db:"notifications"`
	}
	if err := s.db.GetContext(ctx, &row, `
  SELECT
    (SELECT coalesce(json_agg(id ORDER BY id), '[]') FROM posting_windows WHERE ended_at IS NULL)::text AS windows,
    (SELECT coalesce(json_agg(json_build_object(
        'i', k.i, 'id', n.id, 'posting_window_id', n.posting_window_id, 'region', n.region,
        'created_at_us', (extract(epoch FROM n.created_at) * 1000000)::bigint)), '[]')
     FROM unnest($1::text[], $2::timestamptz[]) WITH ORDINALITY AS k(user_id, connected_at, i)
     CROSS JOIN LATERAL (
       SELECT n.id, n.posting_window_id, w.region, n.created_at
       FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
       WHERE n.user_id = k.user_id AND n.created_at > k.connected_at AND w.ended_at IS NULL
       ORDER BY n.created_at, n.id LIMIT 1
     ) n
    )::text AS notifications
 `, userIDs, connectedAts); err != nil {
		return nil, nil, err
	}
	var windows []string
	if err := json.Unmarshal([]byte(row.Windows), &windows); err != nil {
		return nil, nil, err
	}
	var pending []pendingNotificationJSON
	if err := json.Unmarshal([]byte(row.Notifications), &pending); err != nil {
		return nil, nil, err
	}
	results := make([]*getNotificationsResponse, len(keys))
	for _, p := range pending {
		// WITH ORDINALITY は1から数える。
		if p.Index < 1 || p.Index > len(keys) {
			continue
		}
		results[p.Index-1] = &getNotificationsResponse{
			ID: p.ID, PostingWindowID: p.PostingWindowID, Region: p.Region, CreatedAt: time.UnixMicro(p.CreatedAtMicros),
		}
	}
	return windows, results, nil
}

type notificationWaiter struct {
	userID string
	region string
	// 投稿期間が変化したときに通知される。受け取ったら通知テーブルを確認する。
	changed chan struct{}
	// ハブがまとめて引いた通知を渡す。受け取ったらそれを返して閉じる。
	delivered chan getNotificationsResponse
	// ユーザーが退会またはリージョンを変更したときに閉じられる。
	gone     chan struct{}
	goneOnce sync.Once

	mu sync.Mutex
	// 接続した時刻（DB の clock_timestamp）。設定されるまでは、ハブはまとめて引かずに changed で知らせる。
	connectedAt *time.Time
}

func (w *notificationWaiter) signal() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

func (w *notificationWaiter) deliver(notification getNotificationsResponse) {
	select {
	case w.delivered <- notification:
	default:
	}
}

func (w *notificationWaiter) markGone() {
	w.goneOnce.Do(func() { close(w.gone) })
}

// setConnectedAt を呼んだ後は、ハブがこの接続の通知をまとめて引いて渡す。
func (w *notificationWaiter) setConnectedAt(connectedAt time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.connectedAt = &connectedAt
}

func (w *notificationWaiter) key() (notificationKey, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.connectedAt == nil {
		return notificationKey{}, false
	}
	return notificationKey{UserID: w.userID, ConnectedAt: *w.connectedAt}, true
}

type NotificationHub struct {
	source  notificationSource
	mu      sync.Mutex
	waiters map[*notificationWaiter]struct{}
	// 最後に確認できた開いている投稿期間。nil は未確認（またはエラー後）を表し、次回の確認で必ず全員に知らせる。
	// run の goroutine だけが読み書きする。
	windows []string
	// LISTEN で投稿期間の開始を受け取ったときに、すぐ確認させる。
	wake               chan struct{}
	windowPollInterval time.Duration
	// LISTEN で受け取った、退会・リージョンの変更・削除があった利用者の id。runUserChanges が確かめる（#110）。
	userChanges       chan string
	userCheckInterval time.Duration
	// まとめ取りの失敗を最後にログに出した時刻。run の goroutine だけが読み書きする。
	batchErrorLoggedAt time.Time
	// changed で知らされた接続が自分で通知を引くときに使う。source が notificationBatchSource のときだけ設定する。
	fetcher *notificationFetcher
}

func newNotificationHub(source notificationSource) *NotificationHub {
	hub := &NotificationHub{
		source: source, waiters: map[*notificationWaiter]struct{}{},
		wake: make(chan struct{}, 1), windowPollInterval: notificationWindowPollInterval,
		userChanges: make(chan string, userChangeQueueSize), userCheckInterval: notificationUserCheckInterval,
	}
	if batch, ok := source.(notificationBatchSource); ok {
		hub.fetcher = newNotificationFetcher(func(ctx context.Context, keys []notificationKey) ([]*getNotificationsResponse, error) {
			_, pending, err := batch.PendingNotifications(ctx, keys)
			return pending, err
		})
	}
	return hub
}

var (
	notificationHubOnce sync.Once
	// notificationHub はプロセスで1つのハブ。/debug/vars（metrics.go）が別の goroutine から読むので、アトミックに持つ。
	notificationHub atomic.Pointer[NotificationHub]
)

func getNotificationHub(h *HandlerContext) *NotificationHub {
	notificationHubOnce.Do(func() {
		hub := newNotificationHub(dbNotificationSource{db: h.DB})
		notificationHub.Store(hub)
		go hub.run(context.Background())
		go hub.listen(context.Background(), h.Config.DatabaseDSN())
	})
	return notificationHub.Load()
}

func (hub *NotificationHub) subscribe(userID, region string) *notificationWaiter {
	waiter := &notificationWaiter{
		userID: userID, region: region,
		changed: make(chan struct{}, 1), delivered: make(chan getNotificationsResponse, 1), gone: make(chan struct{}),
	}
	hub.mu.Lock()
	hub.waiters[waiter] = struct{}{}
	hub.mu.Unlock()
	return waiter
}

func (hub *NotificationHub) unsubscribe(waiter *notificationWaiter) {
	hub.mu.Lock()
	delete(hub.waiters, waiter)
	hub.mu.Unlock()
}

// notifyWindowChanged は、投稿期間が変わったことを知ったときに呼ぶ。run の goroutine がすぐに確認する。
func (hub *NotificationHub) notifyWindowChanged() {
	select {
	case hub.wake <- struct{}{}:
	default:
	}
}

// notifyUserChanged は、LISTEN で利用者の退会・リージョンの変更・削除を受け取ったときに呼ぶ。
// runUserChanges がその利用者の接続だけを確かめる。ためておける数を超えたら捨てる（ポーリングが拾う）。
func (hub *NotificationHub) notifyUserChanged(userID string) {
	select {
	case hub.userChanges <- userID:
	default:
	}
}

// run は、投稿期間の確認と利用者の確認のループ。利用者の変更を LISTEN で受けたときの確認は、別の goroutine で行う。
// europe と america では、このループは投稿期間のポーリング（100ms ごとに約100ms の往復）でほとんど埋まっているため。
func (hub *NotificationHub) run(ctx context.Context) {
	go hub.runUserChanges(ctx)
	windowTicker := time.NewTicker(hub.windowPollInterval)
	defer windowTicker.Stop()
	userTicker := time.NewTicker(hub.userCheckInterval)
	defer userTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hub.wake:
			hub.checkWindowsNow(ctx)
		case <-windowTicker.C:
			hub.checkWindows(ctx)
		case <-userTicker.C:
			hub.checkUsers(ctx)
		}
	}
}

// waiterCount は、待機中の接続の数を返す（/debug/vars 向け。#108）。
func (hub *NotificationHub) waiterCount() int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.waiters)
}

func (hub *NotificationHub) snapshot() []*notificationWaiter {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	waiters := make([]*notificationWaiter, 0, len(hub.waiters))
	for waiter := range hub.waiters {
		waiters = append(waiters, waiter)
	}
	return waiters
}

// checkWindows は、開いている投稿期間を確認し、変わっていれば待機中の接続に知らせる（ポーリング）。
func (hub *NotificationHub) checkWindows(ctx context.Context) {
	ids, err := hub.source.ActiveWindowIDs(ctx)
	if err != nil {
		hub.windows = nil
		return
	}
	if ids == nil {
		ids = []string{}
	}
	if hub.windows != nil && slices.Equal(hub.windows, ids) {
		return
	}
	if batch, ok := hub.source.(notificationBatchSource); ok {
		hub.deliverPending(ctx, batch)
		return
	}
	hub.windows = ids
	for _, waiter := range hub.snapshot() {
		waiter.signal()
	}
}

// checkWindowsNow は、投稿期間が変わったと分かっているとき（LISTEN で受け取ったとき）に呼ぶ。
// 一覧を比べる往復を省いて、すぐに通知をまとめて引く。
func (hub *NotificationHub) checkWindowsNow(ctx context.Context) {
	if batch, ok := hub.source.(notificationBatchSource); ok {
		hub.deliverPending(ctx, batch)
		return
	}
	hub.checkWindows(ctx)
}

// deliverPending は、待機中の全員の通知を1クエリで引き、見つかった接続に渡す。
//   - 接続した時刻がまだ決まっていない接続には、以前と同じく changed で知らせる（その接続が notificationFetcher で引く）。
//   - 通知が見つからなかった接続は、以前の「引いたが行がなかった」と同じく、そのまま待たせる。
//   - クエリが失敗したら、各接続には知らせず、次回の確認（windowPollInterval 後）でもう一度まとめて引く（#83）。
//     以前は全員に changed で知らせていたので、DB が重いときに、待っている接続の数だけクエリが接続プールを取り合っていた。
//     接続が自分で引くのも同じクエリで、失敗すると接続を閉じてしまうので、失敗が続いてもハブで引き直す。
func (hub *NotificationHub) deliverPending(ctx context.Context, batch notificationBatchSource) {
	waiters := hub.snapshot()
	ready := make([]*notificationWaiter, 0, len(waiters))
	keys := make([]notificationKey, 0, len(waiters))
	for _, waiter := range waiters {
		key, ok := waiter.key()
		if !ok {
			waiter.signal()
			continue
		}
		ready = append(ready, waiter)
		keys = append(keys, key)
	}
	windows, pending, err := batch.PendingNotifications(ctx, keys)
	if err != nil {
		// nil にしておけば、次回の checkWindows は一覧が同じでも必ずまとめて引く。
		hub.windows = nil
		if time.Since(hub.batchErrorLoggedAt) > time.Minute {
			log.Printf("failed to fetch pending notifications (retrying on the next check): %v", err)
			hub.batchErrorLoggedAt = time.Now()
		}
		return
	}
	if windows == nil {
		windows = []string{}
	}
	hub.windows = windows
	for i, notification := range pending {
		if notification != nil {
			ready[i].deliver(*notification)
		}
	}
}

// checkUsers は、待機中の全接続の利用者を確かめ、退会したかリージョンが変わった利用者の接続を閉じる（ポーリング）。
func (hub *NotificationHub) checkUsers(ctx context.Context) {
	hub.checkWaiters(ctx, hub.snapshot())
}

// checkUsersFor は、userIDs の利用者の接続だけを確かめる（LISTEN で変更を受け取ったとき、#110）。判定は checkUsers と同じ。
func (hub *NotificationHub) checkUsersFor(ctx context.Context, userIDs []string) {
	changed := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		changed[id] = true
	}
	waiters := hub.snapshot()
	matched := make([]*notificationWaiter, 0, len(waiters))
	for _, waiter := range waiters {
		if changed[waiter.userID] {
			matched = append(matched, waiter)
		}
	}
	hub.checkWaiters(ctx, matched)
}

// checkWaiters は、waiters の利用者を1クエリで引き、active でないかリージョンが接続時と違う接続を閉じる。
// クエリが失敗したときは何も閉じない（次のポーリングで確かめ直す）。
func (hub *NotificationHub) checkWaiters(ctx context.Context, waiters []*notificationWaiter) {
	if len(waiters) == 0 {
		return
	}
	userIDs := make([]string, 0, len(waiters))
	for _, waiter := range waiters {
		userIDs = append(userIDs, waiter.userID)
	}
	slices.Sort(userIDs)
	regions, err := hub.source.ActiveUserRegions(ctx, slices.Compact(userIDs))
	if err != nil {
		return
	}
	for _, waiter := range waiters {
		if region, ok := regions[waiter.userID]; !ok || region != waiter.region {
			waiter.markGone()
		}
	}
}

// runUserChanges は、LISTEN で届いた利用者の id を受け取り、たまっている分をまとめて1クエリで確かめる。
func (hub *NotificationHub) runUserChanges(ctx context.Context) {
	for {
		var userIDs []string
		select {
		case <-ctx.Done():
			return
		case id := <-hub.userChanges:
			userIDs = append(userIDs, id)
		}
	drain:
		for {
			select {
			case id := <-hub.userChanges:
				userIDs = append(userIDs, id)
			default:
				break drain
			}
		}
		hub.checkUsersFor(ctx, userIDs)
	}
}

// notificationFetcher は、changed で知らされた接続（ハブがまとめて引いたときに、接続した時刻がまだ決まっていなかった接続）が
// 自分で通知を引く問い合わせを、まとめて引く（#83）。以前は接続ごとに1クエリずつ引いていた。
//   - 引いていなければ、すぐに引く。1つだけなら、接続ごとに引いていたときと同じく DB の1往復で返る。
//   - 引いている間に来た問い合わせは、それが終わった後の1回にまとめる。
//
// europe と america では、ハブの goroutine はポーリング（100ms ごとに約100ms の往復）でほとんど埋まっているので、
// ハブに引かせると、ポーリングの終わりを待つ分だけ遅れる。そのため、ハブとは別の goroutine で引く。
type notificationFetcher struct {
	fetch   func(ctx context.Context, keys []notificationKey) ([]*getNotificationsResponse, error)
	mu      sync.Mutex
	running bool
	queue   []*notificationFetchRequest
}

type notificationFetchRequest struct {
	ctx    context.Context
	key    notificationKey
	result chan notificationFetchResult
}

type notificationFetchResult struct {
	notification *getNotificationsResponse
	err          error
}

// fetch には、keys のそれぞれについて getNotificationsHandler が1件目に返す通知を、keys と同じ順に返す関数を渡す（ないキーは nil）。
func newNotificationFetcher(fetch func(ctx context.Context, keys []notificationKey) ([]*getNotificationsResponse, error)) *notificationFetcher {
	return &notificationFetcher{fetch: fetch}
}

// get は key の通知を返す。なければ nil を返す。
func (f *notificationFetcher) get(ctx context.Context, key notificationKey) (*getNotificationsResponse, error) {
	request := &notificationFetchRequest{ctx: ctx, key: key, result: make(chan notificationFetchResult, 1)}
	f.mu.Lock()
	f.queue = append(f.queue, request)
	if !f.running {
		f.running = true
		go f.run()
	}
	f.mu.Unlock()
	select {
	case result := <-request.result:
		return result.notification, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run は、たまった問い合わせがなくなるまで、まとめて引いて返す。
// 接続が閉じた問い合わせは引かない。どの接続のものでもないので、クエリは接続の context では打ち切らない。
func (f *notificationFetcher) run() {
	for {
		f.mu.Lock()
		requests := slices.DeleteFunc(f.queue, func(r *notificationFetchRequest) bool { return r.ctx.Err() != nil })
		f.queue = nil
		if len(requests) == 0 {
			f.running = false
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
		keys := make([]notificationKey, len(requests))
		for i, request := range requests {
			keys[i] = request.key
		}
		pending, err := f.fetch(context.Background(), keys)
		if err == nil && len(pending) != len(keys) {
			err = fmt.Errorf("pending notifications: got %d results for %d keys", len(pending), len(keys))
		}
		for i, request := range requests {
			if err != nil {
				request.result <- notificationFetchResult{err: err}
			} else {
				request.result <- notificationFetchResult{notification: pending[i]}
			}
		}
	}
}

// listen は、プールとは別の専用の接続で、投稿期間の開始と利用者の変更の pg_notify を待つ。
// 投稿期間の開始を受け取ったらハブにすぐ確認させ、利用者の変更を受け取ったらその利用者の接続だけを確かめさせる（#110）。
// 接続が切れたらつなぎ直す。つなぎ直した直後も、切れている間の取りこぼしに備えて確認させる
// （利用者の変更の取りこぼしは、ポーリングが拾う）。切れている間も、ポーリングで検知できる。
func (hub *NotificationHub) listen(ctx context.Context, dsn string) {
	for ctx.Err() == nil {
		if err := hub.listenOnce(ctx, dsn); err != nil && ctx.Err() == nil {
			log.Printf("posting window listener stopped: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(postingWindowListenRetry):
		}
	}
}

func (hub *NotificationHub) listenOnce(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, notificationListenStatement); err != nil {
		return err
	}
	hub.notifyWindowChanged()
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		switch notification.Channel {
		case userChangeNotifyChannel:
			hub.notifyUserChanged(notification.Payload)
		default:
			hub.notifyWindowChanged()
		}
	}
}
