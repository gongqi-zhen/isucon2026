package isureal

// nullableUser は、LEFT JOIN で NULL になりうる users の列（userColumns と同じ）を受け取る。
type nullableUser struct {
	ID             *string `db:"id"`
	Username       *string `db:"username"`
	SecretKeyHash  *string `db:"secret_key_hash"`
	DisplayName    *string `db:"display_name"`
	Bio            *string `db:"bio"`
	ProfileImageID *string `db:"profile_image_id"`
	Region         *string `db:"region"`
	AccountType    *string `db:"account_type"`
	Status         *string `db:"status"`
}

// user は、NULL でない行を User にし、画像署名の鍵を設定する。
func (u nullableUser) user() (User, error) {
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	user := User{
		ID: deref(u.ID), Username: deref(u.Username), SecretKeyHash: deref(u.SecretKeyHash),
		DisplayName: deref(u.DisplayName), Bio: deref(u.Bio), ProfileImageID: u.ProfileImageID,
		Region: deref(u.Region), AccountType: deref(u.AccountType), Status: deref(u.Status),
	}
	return user, user.setImageKey()
}
