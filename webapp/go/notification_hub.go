package isureal

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// 投稿期間の開始は別リージョンのサーバに届くことがあるため、変化は DB をポーリングして検知する。
// 接続ごとにポーリングせず、プロセスで1本の goroutine が開いている投稿期間と待機中ユーザーの状態をまとめて確認する。
const (
	notificationWindowPollInterval = 100 * time.Millisecond
	notificationUserCheckInterval  = 500 * time.Millisecond
)

type notificationSource interface {
	// 開いている投稿期間のIDを返す。
	ActiveWindowIDs(ctx context.Context) ([]string, error)
	// 指定ユーザーのうち active なユーザーのリージョンを返す。
	ActiveUserRegions(ctx context.Context, userIDs []string) (map[string]string, error)
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

type notificationWaiter struct {
	userID string
	region string
	// 投稿期間が変化したときに通知される。受け取ったら通知テーブルを確認する。
	changed chan struct{}
	// ユーザーが退会またはリージョンを変更したときに閉じられる。
	gone     chan struct{}
	goneOnce sync.Once
}

func (w *notificationWaiter) signal() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

func (w *notificationWaiter) markGone() {
	w.goneOnce.Do(func() { close(w.gone) })
}

type NotificationHub struct {
	source  notificationSource
	mu      sync.Mutex
	waiters map[*notificationWaiter]struct{}
	// 最後に確認できた開いている投稿期間。nil は未確認（またはエラー後）を表し、次回の確認で必ず全員に知らせる。
	windows []string
}

func newNotificationHub(source notificationSource) *NotificationHub {
	return &NotificationHub{source: source, waiters: map[*notificationWaiter]struct{}{}}
}

var (
	notificationHubOnce sync.Once
	notificationHub     *NotificationHub
)

func getNotificationHub(db *sqlx.DB) *NotificationHub {
	notificationHubOnce.Do(func() {
		notificationHub = newNotificationHub(dbNotificationSource{db: db})
		go notificationHub.run(context.Background())
	})
	return notificationHub
}

func (hub *NotificationHub) subscribe(userID, region string) *notificationWaiter {
	waiter := &notificationWaiter{userID: userID, region: region, changed: make(chan struct{}, 1), gone: make(chan struct{})}
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

func (hub *NotificationHub) run(ctx context.Context) {
	windowTicker := time.NewTicker(notificationWindowPollInterval)
	defer windowTicker.Stop()
	userTicker := time.NewTicker(notificationUserCheckInterval)
	defer userTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-windowTicker.C:
			hub.checkWindows(ctx)
		case <-userTicker.C:
			hub.checkUsers(ctx)
		}
	}
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
	hub.windows = ids
	for _, waiter := range hub.snapshot() {
		waiter.signal()
	}
}

func (hub *NotificationHub) checkUsers(ctx context.Context) {
	waiters := hub.snapshot()
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
