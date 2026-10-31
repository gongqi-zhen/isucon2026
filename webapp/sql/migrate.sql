-- 既存のDBに対して冪等に適用するマイグレーション。
-- init.sh が reset.sql の前に毎回実行する。初回はインデックス作成に時間がかかるため、
-- 負荷走行の前に一度手動で実行しておくこと。

-- ユーザー検索用の正規化済みテキスト。
-- 中身は normalizeSearchText(username) || E'\n' || normalizeSearchText(display_name)（Go と同じ正規化）。
-- NULL は「SQL で正規化できなかった」ことを表し、検索時は常に候補に含めて Go 側で判定する。
ALTER TABLE users ADD COLUMN IF NOT EXISTS search_text text;

-- Go の NFC + case fold + NFC と結果が一致することが保証できる文字だけで構成される場合に限り、
-- SQL 側で正規化済みテキストを返す。それ以外は NULL を返す。
-- 対象の文字（いずれも NFC で変化せず、case fold の対象は ASCII の A-Z だけ）:
--   ASCII 印字可能文字、CJK 記号（結合文字 U+302A-302F を除く）、ひらがな（結合用濁点 U+3099/309A を除く）、
--   カタカナ、CJK 統合漢字、ハングル音節、半角カナ
CREATE OR REPLACE FUNCTION isureal_search_text(username text, display_name text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT CASE
    WHEN username ~ '^[a-z0-9_.-]+$'
     AND display_name ~ '^[ -~　-〩〰-〿ぁ-ゖ゛-ヿ一-鿿가-힣｡-ﾟ]*$'
    THEN username || E'\n' || translate(display_name, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz')
  END
$$;

UPDATE users SET search_text = isureal_search_text(username, display_name)
WHERE search_text IS NULL AND isureal_search_text(username, display_name) IS NOT NULL;

-- pg_trgm が使えない環境でも初期化を失敗させない（検索は正しく動き、インデックスが効かないだけ）。
DO $$
BEGIN
  CREATE EXTENSION IF NOT EXISTS pg_trgm;
  EXECUTE 'CREATE INDEX IF NOT EXISTS users_search_text_trgm ON users USING gin (search_text gin_trgm_ops)';
EXCEPTION WHEN OTHERS THEN
  RAISE WARNING 'pg_trgm is unavailable, skipping users_search_text_trgm: %', SQLERRM;
END $$;

-- 001-schema.sql と同じインデックス（既存のDB向け）
CREATE INDEX IF NOT EXISTS users_username_id ON users(username, id);
-- 登録時のユーザー名の重複を、テーブルロックではなく一意制約で防ぐ（#25）。
CREATE UNIQUE INDEX IF NOT EXISTS users_username_key ON users(username);
CREATE INDEX IF NOT EXISTS users_search_text_null ON users(username, id) WHERE search_text IS NULL;
CREATE INDEX IF NOT EXISTS login_sessions_token_hash_active ON login_sessions(token_hash) WHERE active = true;
CREATE INDEX IF NOT EXISTS login_sessions_user_id_active ON login_sessions(user_id) WHERE active = true;
CREATE INDEX IF NOT EXISTS administrator_sessions_token_hash_active ON administrator_sessions(token_hash) WHERE active = true;
CREATE INDEX IF NOT EXISTS friendships_user_high_id ON friendships(user_high_id);
CREATE INDEX IF NOT EXISTS follows_followee_id ON follows(followee_id);
CREATE INDEX IF NOT EXISTS block_relations_blocked_id ON block_relations(blocked_id);
CREATE INDEX IF NOT EXISTS friend_requests_requester_pending ON friend_requests(requester_id, recipient_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS friend_requests_recipient_pending ON friend_requests(recipient_id, requester_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS posts_author_id_posting_window_id ON posts(author_id, posting_window_id);
CREATE INDEX IF NOT EXISTS posts_image_id ON posts(image_id);
CREATE INDEX IF NOT EXISTS realmojis_image_id ON realmojis(image_id);
CREATE INDEX IF NOT EXISTS comments_post_id_created_at ON comments(post_id, created_at, id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS notifications_user_id_created_at ON notifications(user_id, created_at, id);
