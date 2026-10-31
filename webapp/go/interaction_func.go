package isureal

// 投稿・RealMoji・コメントの関数（webapp/sql/interaction_functions.sql、#50）が返す行。
// 関数は autocommit の1文で呼ぶので、ロックを持つのは関数の実行中だけになる。

// funcUser は、関数が返す行為者の users の列。
type funcUser struct {
	ID             *string `db:"out_user_id"`
	Username       *string `db:"out_username"`
	SecretKeyHash  *string `db:"out_secret_key_hash"`
	DisplayName    *string `db:"out_display_name"`
	Bio            *string `db:"out_bio"`
	ProfileImageID *string `db:"out_profile_image_id"`
	Region         *string `db:"out_region"`
	AccountType    *string `db:"out_account_type"`
	Status         *string `db:"out_status"`
}

func (u funcUser) user() User {
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return User{
		ID: deref(u.ID), Username: deref(u.Username), SecretKeyHash: deref(u.SecretKeyHash),
		DisplayName: deref(u.DisplayName), Bio: deref(u.Bio), ProfileImageID: u.ProfileImageID,
		Region: deref(u.Region), AccountType: deref(u.AccountType), Status: deref(u.Status),
	}
}

const funcUserColumns = `out_user_id, out_username, out_secret_key_hash, out_display_name, out_bio,
  out_profile_image_id, out_region, out_account_type, out_status`
