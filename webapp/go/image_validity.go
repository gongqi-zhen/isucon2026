package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// 画像の有効性を、無効化の一括検知で長く使う（#64）。
//
// europe と america では、画像の有効性を確かめる1往復に約100ms かかる。
// そこで、DB で有効と確かめた画像を覚えておき、無効になったことを webapp/sql/image_invalidation.sql の
// image_invalidations から1本の goroutine でまとめて検知して消す。覚えている画像の取得では DB を引かない。
//
// 「投稿の削除成功から5秒を過ぎたら取得できない」ことは、次の2つで保つ（時計はこのプロセスの単調時計だけを使う）。
//
//  1. 読み込みの連鎖：各回の読み込みは、前回のスナップショットの xmin 以上の xid の行を読む。
//     前回のスナップショットに含まれないトランザクションの xid は xmin 以上なので、コミットした無効化は、
//     それを含む最初のスナップショットの読み込みで必ず読める（SQL のファイルの説明を参照）。
//  2. 鮮度：覚えている画像を使うのは、最後に反映した読み込みを送った時刻が imageInvalidationTrustWindow 以内のときだけ。
//
// 削除のコミットを c、その応答を r、5秒後の取得を t >= r + 5s とする。最後に反映した読み込みを送った時刻 p は
// p >= t - imageInvalidationTrustWindow > t - 5s >= r >= c なので、そのスナップショットは削除を含み、
// 1 により、その読み込みまでに削除を反映している。読み込みが失敗し続けると 2 を満たさなくなり、
// これまでと同じく imageValidityTTL ごとに DB で確かめる。
//
// 覚え始めるのは、連鎖の起点（起動や初期化の後、最初の読み込みの結果を受け取った時刻 = baselineAt）より後に
// 送ったクエリで有効と確かめた画像だけ。それより前に確かめた画像は、起点の読み込みより前の削除を見ていないかもしれない。
// また、有効と確かめたクエリを送った後に、同じ画像（持ち主、投稿）の無効化を反映していたら、その結果は覚えない。
const (
	imageInvalidationPollInterval = 500 * time.Millisecond
	imageInvalidationPollTimeout  = 2 * time.Second
	imageInvalidationTrustWindow  = 3 * time.Second
	// 反映した無効化を、遅れて届いた確認の結果と照らすために残しておく時間。これより前に送った確認の結果は覚えない。
	imageInvalidationKeep   = 30 * time.Second
	imageValidityMaxEntries = 500_000
)

// imageValidityMeta は、画像が有効である理由に関わる id。これらの無効化を検知したら、その画像を忘れる。
type imageValidityMeta struct {
	OwnerID string
	// 投稿の画像はその投稿、RealMoji の画像はリアクションした投稿。
	PostIDs []string
	// RealMoji の画像の、リアクションした投稿の作者。
	PostAuthorIDs []string
}

func (meta imageValidityMeta) userIDs() []string {
	return append([]string{meta.OwnerID}, meta.PostAuthorIDs...)
}

type trackedImage struct {
	meta        imageValidityMeta
	validatedAt time.Time
}

// imageInvalidationPoll は1回の読み込みの結果。
type imageInvalidationPoll struct {
	Epoch string
	XMin  string
	Rows  []imageInvalidationRow
}

type imageInvalidationRow struct {
	ImageID, UserID, PostID string
}

type imageInvalidationSource interface {
	// xmin が空なら行は読まず、世代とスナップショットの xmin だけを返す。
	poll(ctx context.Context, xmin string) (imageInvalidationPoll, error)
}

type dbImageInvalidationSource struct{ db *sqlx.DB }

func (s dbImageInvalidationSource) poll(ctx context.Context, xmin string) (imageInvalidationPoll, error) {
	var row struct {
		Epoch string `db:"epoch"`
		XMin  string `db:"xmin"`
		Rows  []byte `db:"rows"`
	}
	// 1文なので、行もスナップショットの xmin も、同じスナップショットで読む。
	if err := s.db.GetContext(ctx, &row, `
  SELECT e.epoch, pg_snapshot_xmin(pg_current_snapshot())::text AS xmin,
    COALESCE((
      SELECT json_agg(json_build_array(coalesce(i.image_id, ''), coalesce(i.user_id, ''), coalesce(i.post_id, '')))
      FROM image_invalidations i WHERE i.xid >= NULLIF($1, '')::xid8
    ), '[]') AS rows
  FROM image_invalidation_epoch e WHERE e.id = 1
 `, xmin); err != nil {
		return imageInvalidationPoll{}, err
	}
	var raw [][3]string
	if err := json.Unmarshal(row.Rows, &raw); err != nil {
		return imageInvalidationPoll{}, err
	}
	result := imageInvalidationPoll{Epoch: row.Epoch, XMin: row.XMin, Rows: make([]imageInvalidationRow, 0, len(raw))}
	for _, r := range raw {
		result.Rows = append(result.Rows, imageInvalidationRow{ImageID: r[0], UserID: r[1], PostID: r[2]})
	}
	return result, nil
}

type imageValidityTracker struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*trackedImage
	byUser  map[string]map[string]struct{}
	byPost  map[string]map[string]struct{}
	// 無効化を反映した読み込みの結果を受け取った時刻。
	invalidatedImages map[string]time.Time
	invalidatedUsers  map[string]time.Time
	invalidatedPosts  map[string]time.Time
	// ゼロ値は、連鎖の起点がまだないことを表す。
	baselineAt     time.Time
	lastPollSentAt time.Time
	epoch          string
	xmin           string
	start          sync.Once
}

func newImageValidityTracker() *imageValidityTracker {
	tracker := &imageValidityTracker{now: time.Now}
	tracker.clearLocked()
	return tracker
}

var imageValidity = newImageValidityTracker()

// startImageInvalidationPoller は、最初に画像を返すときに1回だけ読み込みのループを始める。
func startImageInvalidationPoller(db *sqlx.DB) {
	imageValidity.start.Do(func() {
		go imageValidity.run(context.Background(), dbImageInvalidationSource{db: db})
	})
}

func (t *imageValidityTracker) clearLocked() {
	t.entries = map[string]*trackedImage{}
	t.byUser = map[string]map[string]struct{}{}
	t.byPost = map[string]map[string]struct{}{}
	t.invalidatedImages = map[string]time.Time{}
	t.invalidatedUsers = map[string]time.Time{}
	t.invalidatedPosts = map[string]time.Time{}
	t.baselineAt = time.Time{}
}

// reset は初期化で呼ぶ。覚えている画像を捨て、次の読み込みを新しい起点にする。
func (t *imageValidityTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearLocked()
}

// valid は、覚えている画像をそのまま有効とみなしてよいかを返す。
func (t *imageValidityTracker) valid(imageID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.baselineAt.IsZero() || t.now().Sub(t.lastPollSentAt) > imageInvalidationTrustWindow {
		return false
	}
	entry, ok := t.entries[imageID]
	return ok && !entry.validatedAt.Before(t.baselineAt)
}

// add は、sentAt に送ったクエリで有効と確かめた画像を覚える。
func (t *imageValidityTracker) add(imageID string, meta imageValidityMeta, sentAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.baselineAt.IsZero() || sentAt.Before(t.baselineAt) || t.now().Sub(sentAt) >= imageInvalidationKeep {
		return
	}
	// 確かめた後に無効化を反映していたら、その確認の結果は古いかもしれない。
	if t.invalidatedImages[imageID].After(sentAt) {
		return
	}
	for _, id := range meta.userIDs() {
		if t.invalidatedUsers[id].After(sentAt) {
			return
		}
	}
	for _, id := range meta.PostIDs {
		if t.invalidatedPosts[id].After(sentAt) {
			return
		}
	}
	if entry, ok := t.entries[imageID]; ok {
		t.removeLocked(imageID, entry)
	} else if len(t.entries) >= imageValidityMaxEntries {
		return
	}
	t.entries[imageID] = &trackedImage{meta: meta, validatedAt: sentAt}
	for _, id := range meta.userIDs() {
		addToIndex(t.byUser, id, imageID)
	}
	for _, id := range meta.PostIDs {
		addToIndex(t.byPost, id, imageID)
	}
}

func addToIndex(index map[string]map[string]struct{}, key, imageID string) {
	if key == "" {
		return
	}
	set, ok := index[key]
	if !ok {
		set = map[string]struct{}{}
		index[key] = set
	}
	set[imageID] = struct{}{}
}

func removeFromIndex(index map[string]map[string]struct{}, key, imageID string) {
	if set, ok := index[key]; ok {
		delete(set, imageID)
		if len(set) == 0 {
			delete(index, key)
		}
	}
}

func (t *imageValidityTracker) removeLocked(imageID string, entry *trackedImage) {
	delete(t.entries, imageID)
	for _, id := range entry.meta.userIDs() {
		removeFromIndex(t.byUser, id, imageID)
	}
	for _, id := range entry.meta.PostIDs {
		removeFromIndex(t.byPost, id, imageID)
	}
}

// forgetImageLocked は、画像を一括検知からも、imageValidityTTL の間だけ使う確認の結果（imageValidityCache）からも消す。
// imageValidityCache の結果も削除の5秒後には使わないが、無効化を検知したら、すぐに DB で確かめ直すようにする。
func (t *imageValidityTracker) forgetImageLocked(imageID string) {
	imageValidityCache.Remove(imageID)
	if entry, ok := t.entries[imageID]; ok {
		t.removeLocked(imageID, entry)
	}
}

// apply は、sentAt に送って receivedAt に受け取った読み込みの結果を反映する。
func (t *imageValidityTracker) apply(result imageInvalidationPoll, sentAt, receivedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if result.Epoch != t.epoch {
		// 初期化（または起動）の後の最初の読み込み。覚えている画像を捨てて、ここを起点にする。
		t.clearLocked()
		t.epoch = result.Epoch
	}
	if t.baselineAt.IsZero() {
		t.baselineAt = receivedAt
	}
	for _, row := range result.Rows {
		if row.ImageID != "" {
			t.forgetImageLocked(row.ImageID)
			t.invalidatedImages[row.ImageID] = receivedAt
		}
		if row.UserID != "" {
			for imageID := range t.byUser[row.UserID] {
				t.forgetImageLocked(imageID)
			}
			t.invalidatedUsers[row.UserID] = receivedAt
		}
		if row.PostID != "" {
			for imageID := range t.byPost[row.PostID] {
				t.forgetImageLocked(imageID)
			}
			t.invalidatedPosts[row.PostID] = receivedAt
		}
	}
	for _, invalidated := range []map[string]time.Time{t.invalidatedImages, t.invalidatedUsers, t.invalidatedPosts} {
		for id, at := range invalidated {
			if receivedAt.Sub(at) > imageInvalidationKeep {
				delete(invalidated, id)
			}
		}
	}
	t.xmin = result.XMin
	t.lastPollSentAt = sentAt
}

// pollOnce は1回読み込んで反映する。失敗したときは何も変えない（鮮度が切れれば、DB で確かめる方法に戻る）。
func (t *imageValidityTracker) pollOnce(ctx context.Context, source imageInvalidationSource) error {
	t.mu.Lock()
	xmin := t.xmin
	t.mu.Unlock()
	sentAt := t.now()
	ctx, cancel := context.WithTimeout(ctx, imageInvalidationPollTimeout)
	defer cancel()
	result, err := source.poll(ctx, xmin)
	if err != nil {
		return err
	}
	t.apply(result, sentAt, t.now())
	return nil
}

func (t *imageValidityTracker) run(ctx context.Context, source imageInvalidationSource) {
	ticker := time.NewTicker(imageInvalidationPollInterval)
	defer ticker.Stop()
	var lastLogged time.Time
	for {
		if err := t.pollOnce(ctx, source); err != nil && time.Since(lastLogged) > time.Minute {
			// image_invalidation.sql を流していない DB でも動く（毎回 DB で確かめる）。ログは1分に1回だけ出す。
			log.Printf("image invalidation poll failed (falling back to per-request checks): %v", err)
			lastLogged = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// imageValidityColumns は、users u を持ち主として、画像 $1 が有効である理由を返す列。
// 元の validImageFrom と同じ条件：プロフィール画像、削除されていない投稿の画像、
// 差し替え・削除されていない RealMoji の画像（投稿が削除されておらず、投稿の作者が active）。
const imageValidityColumns = `u.id AS owner_id,
    coalesce(u.profile_image_id = $1, false) AS profile,
    pi.id AS post_id, ri.post_id AS realmoji_post_id, ri.author_id AS realmoji_post_author_id`

const imageValidityJoins = `
   LEFT JOIN LATERAL (
     SELECT p.id FROM posts p
     WHERE p.image_id = $1 AND p.author_id = u.id AND p.deleted_at IS NULL LIMIT 1
   ) pi ON true
   LEFT JOIN LATERAL (
     SELECT r.post_id, p.author_id FROM realmojis r
     JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
     JOIN users author ON author.id = p.author_id AND author.status = 'active'
     WHERE r.image_id = $1 AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL LIMIT 1
   ) ri ON true`

type imageValidityRow struct {
	OwnerID              string  `db:"owner_id"`
	Profile              bool    `db:"profile"`
	PostID               *string `db:"post_id"`
	RealMojiPostID       *string `db:"realmoji_post_id"`
	RealMojiPostAuthorID *string `db:"realmoji_post_author_id"`
}

func (row imageValidityRow) valid() bool {
	return row.Profile || row.PostID != nil || row.RealMojiPostID != nil
}

func (row imageValidityRow) meta() imageValidityMeta {
	meta := imageValidityMeta{OwnerID: row.OwnerID}
	if row.PostID != nil {
		meta.PostIDs = append(meta.PostIDs, *row.PostID)
	}
	if row.RealMojiPostID != nil {
		meta.PostIDs = append(meta.PostIDs, *row.RealMojiPostID)
	}
	if row.RealMojiPostAuthorID != nil {
		meta.PostAuthorIDs = append(meta.PostAuthorIDs, *row.RealMojiPostAuthorID)
	}
	return meta
}

// checkImageValidity は、画像の中身をキャッシュしているときに、画像が今も有効かを DB で確かめる。
// 持ち主が分かっていれば image_objects を読まない（全パーティションの索引を調べずに済む）。
// 画像の行は、参照している投稿・RealMoji・プロフィールがある間は消えないので、行の有無は確かめなくても結果は同じ。
func checkImageValidity(ctx context.Context, db sqlx.QueryerContext, imageID, ownerID string) (imageValidityRow, bool, error) {
	var row imageValidityRow
	err := sqlx.GetContext(ctx, db, &row, `
   SELECT `+imageValidityColumns+`
   FROM users u`+imageValidityJoins+`
   WHERE u.id = COALESCE(NULLIF($2, ''), (SELECT i.owner_user_id FROM image_objects i WHERE i.id = $1 LIMIT 1))
     AND u.status = 'active'
  `, imageID, ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	return row, row.valid(), nil
}

type imageObjectValidityRow struct {
	imageValidityRow
	Data      []byte `db:"data"`
	MediaType string `db:"media_type"`
}

// loadValidImageObject は、有効な画像の中身と、有効である理由を1回で読む。有効でなければ sql.ErrNoRows を返す。
func loadValidImageObject(ctx context.Context, db sqlx.QueryerContext, imageID string) (imageObjectValidityRow, error) {
	var row imageObjectValidityRow
	err := sqlx.GetContext(ctx, db, &row, `
   SELECT i.data, i.media_type, `+imageValidityColumns+`
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'`+imageValidityJoins+`
   WHERE i.id = $1 AND (u.profile_image_id = i.id OR pi.id IS NOT NULL OR ri.post_id IS NOT NULL)
   LIMIT 1
  `, imageID)
	return row, err
}

// markImageValid は、sentAt に送ったクエリで有効と確かめた画像を、両方のキャッシュに入れる。
func markImageValid(imageID string, meta imageValidityMeta, sentAt time.Time) {
	imageValidityCache.Add(imageID, struct{}{}, 1, sentAt.Add(imageValidityTTL))
	imageValidity.add(imageID, meta, sentAt)
}

// imageKnownValid は、DB を引かずに有効とみなしてよいかを返す。
func imageKnownValid(imageID string) bool {
	if imageValidity.valid(imageID) {
		return true
	}
	_, ok := imageValidityCache.Get(imageID)
	return ok
}
