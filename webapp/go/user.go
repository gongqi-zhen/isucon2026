package isureal

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
)

type User struct {
	ID             string  `db:"id" json:"id"`
	Username       string  `db:"username" json:"username"`
	SecretKeyHash  string  `db:"secret_key_hash" json:"-"`
	DisplayName    string  `db:"display_name" json:"displayName"`
	Bio            string  `db:"bio" json:"bio"`
	ProfileImageID *string `db:"profile_image_id" json:"-"`
	Region         string  `db:"region" json:"region"`
	AccountType    string  `db:"account_type" json:"accountType"`
	Status         string  `db:"status" json:"status"`
	ImageKey       []byte  `db:"-" json:"-"`
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

func validUserText(value string, minimum, maximum int) bool {
	length := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && length >= minimum && length <= maximum
}

func validRegion(region string) bool {
	switch region {
	case "america", "europe", "asia":
		return true
	default:
		return false
	}
}

var userColumns = []string{"id", "username", "secret_key_hash", "display_name", "bio", "profile_image_id", "region", "account_type", "status"}

func userColumnsAs(alias, prefix string) string {
	columns := make([]string, len(userColumns))
	for i, column := range userColumns {
		columns[i] = fmt.Sprintf(`%s.%s AS "%s.%s"`, alias, column, prefix, column)
	}
	return strings.Join(columns, ", ")
}

func qualifiedUserColumns(alias string) string {
	columns := make([]string, len(userColumns))
	for i, column := range userColumns {
		columns[i] = alias + "." + column
	}
	return strings.Join(columns, ", ")
}

// lockUser は users の行を FOR NO KEY UPDATE でロックし、同時にその行を返す。
// status による絞り込みはしないので、active であるかは呼び出し側で activeUser を使って確認する。
func lockUser(ctx context.Context, tx *sqlx.Tx, userID string) (User, error) {
	var user User
	err := tx.GetContext(ctx, &user, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID)
	return user, err
}

// lockUsers は複数の users の行を id の昇順（Go の文字列比較と同じバイト順）に FOR NO KEY UPDATE でロックし、id ごとの行を返す。
// どれか1件でも見つからなければ sql.ErrNoRows を返す。status による絞り込みはしない。
func lockUsers(ctx context.Context, tx *sqlx.Tx, userIDs []string) (map[string]User, error) {
	var users []User
	if err := tx.SelectContext(ctx, &users, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = ANY($1::text[]) ORDER BY id COLLATE "C" FOR NO KEY UPDATE`, userIDs); err != nil {
		return nil, err
	}
	locked := make(map[string]User, len(users))
	for _, user := range users {
		locked[user.ID] = user
	}
	for _, id := range userIDs {
		if _, ok := locked[id]; !ok {
			return nil, sql.ErrNoRows
		}
	}
	return locked, nil
}

// lockInteractionUsers は、投稿へのコメントや RealMoji のために、行為者を FOR NO KEY UPDATE、投稿者を FOR SHARE でロックし、id ごとの行を返す。
// 同じ行為者の操作どうしは直列になり、同じ投稿者への別々の行為者からの操作は並行して進む。
// 投稿者の FOR SHARE は、ブロックや友達関係の変更、投稿の編集・削除（どれも FOR NO KEY UPDATE を取る）とは衝突するので、それらとの順序は今まで通り保たれる。
// デッドロックを避けるため、lockUsers と同じく id の昇順（COLLATE "C"）にロックする。LATERAL で外側の行を先にロックさせ、1往復で済ませる。
// どちらかが見つからなければ sql.ErrNoRows を返す。status による絞り込みはしない。
func lockInteractionUsers(ctx context.Context, tx *sqlx.Tx, actor, author string) (map[string]User, error) {
	if actor == author {
		return lockUsers(ctx, tx, []string{actor})
	}
	low, high := min(actor, author), max(actor, author)
	lowMode, highMode := "FOR SHARE", "FOR SHARE"
	if low == actor {
		lowMode = "FOR NO KEY UPDATE"
	} else {
		highMode = "FOR NO KEY UPDATE"
	}
	columns := strings.Join(userColumns, ", ")
	var row struct {
		Low  User `db:"low"`
		High User `db:"high"`
	}
	err := tx.GetContext(ctx, &row, `
  SELECT `+userColumnsAs("l", "low")+`, `+userColumnsAs("h", "high")+`
  FROM (SELECT `+columns+` FROM users WHERE id = $1 `+lowMode+`) l,
  LATERAL (SELECT `+columns+` FROM users WHERE id = $2 AND l.id IS NOT NULL `+highMode+`) h
 `, low, high)
	if err != nil {
		return nil, err
	}
	return map[string]User{row.Low.ID: row.Low, row.High.ID: row.High}, nil
}

// activeUser は getUser と同じく、active でなければ sql.ErrNoRows を返し、画像署名の鍵を設定する。
func activeUser(user User) (User, error) {
	if user.Status != "active" {
		return user, sql.ErrNoRows
	}
	return user, user.setImageKey()
}

func (user *User) setImageKey() error {
	key, err := imageSigningKey(user.SecretKeyHash)
	user.ImageKey = key
	return err
}

func getUser(ctx context.Context, db sqlx.QueryerContext, userID string) (User, error) {
	var user User
	err := sqlx.GetContext(ctx, db, &user, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = $1 AND status = 'active'`, userID)
	if err != nil {
		return user, err
	}
	return user, user.setImageKey()
}
