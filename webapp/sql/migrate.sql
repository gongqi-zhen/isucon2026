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

-- 初期化（reset.sql）で「初期データより後に追加された行」を消すときに使う（#41）。
-- created_at は追記順に増えるので、挿入時のインデックス更新は右端への追記で済む。
CREATE INDEX IF NOT EXISTS notifications_created_at ON notifications(created_at);
CREATE INDEX IF NOT EXISTS comments_created_at ON comments(created_at);
CREATE INDEX IF NOT EXISTS realmojis_created_at ON realmojis(created_at);
CREATE INDEX IF NOT EXISTS posts_created_at ON posts(created_at);

-- 通知は、投稿期間の開始時にそのリージョンの全利用者分を1文でまとめて挿入する（#41）。
-- 次の制約は、行ごとにインデックスのばらばらのページを読み書きさせるので外す。整合は挿入する側で保たれている。
-- - 外部キー：user_id は同じ文で users から選び、posting_window_id は同じトランザクションで作った投稿期間。
--   行ごとのトリガが users と posting_windows を確認し、users の行に共有ロックも付ける。
-- - (user_id, posting_window_id) の一意制約：投稿期間の開始は advisory lock で直列化され、
--   新しく作った投稿期間に1回だけ挿入するので、重複は起こらない。
--   通知の検索は notifications_user_id_created_at を使う。
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_user_id_fkey;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_posting_window_id_fkey;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_user_id_posting_window_id_key;

-- 友達関係の操作を、ロックの取得から確定までを DB の中で行う関数にする（#33 案A）。
-- アプリは autocommit の1文で呼ぶので、users 行のロックを持つのは関数の実行中だけになり、
-- europe・america からの往復（約100ms）の間ロックを持ち続けることがなくなる。
-- 結果は 'ok'、'not_found'（404 relationship_not_found）、'conflict'（409 relationship_conflict）、
-- 'forbidden'（403 relationship_action_forbidden）のいずれかで返す。
-- 関数の中の文はそれぞれ新しいスナップショットで動くので、ロックの後の確認はロック後の状態を見る。

-- 2人の users 行を、Go の文字列比較と同じ順（COLLATE "C"）で FOR NO KEY UPDATE ロックする。
-- 2人とも active ならロックして true を返す。
-- 友達の解除、ブロック、ブロックの解除で使う。友達の解除とブロックは閲覧やコメントの権限を減らすので、
-- 権限を確かめるコメント・RealMoji（投稿者を FOR SHARE）と直列にする必要がある。
CREATE OR REPLACE FUNCTION isureal_lock_user_pair(a text, b text) RETURNS boolean
LANGUAGE plpgsql AS $$
DECLARE
  n int;
BEGIN
  SELECT count(*) INTO n FROM (
    SELECT u.id FROM users u WHERE u.id IN (a, b) AND u.status = 'active' ORDER BY u.id COLLATE "C" FOR NO KEY UPDATE
  ) locked;
  RETURN n = 2;
END $$;

-- 行為者の users 行を FOR NO KEY UPDATE、相手の行を FOR SHARE で、isureal_lock_user_pair と同じ順（COLLATE "C"）にロックする（#87）。
-- 2人とも active ならロックして true を返す。同じ人どうしなら、isureal_lock_user_pair と同じく false を返す。
-- フォロー・フォローの解除・友達申請・申請の承認・拒否・取り下げで使う。どれも閲覧やコメントの権限を減らさない。
-- - 相手の FOR SHARE は、相手が関わるブロックや友達の解除（isureal_lock_user_pair）、相手の退会や認定（UPDATE users）とはぶつかるので、
--   それらとの順序は今まで通り保たれる。
-- - ほかの行為者からの同じ相手への操作や、相手の投稿へのコメント・RealMoji（投稿者を FOR SHARE）とはぶつからず、並行して進む。
-- - 同じ行為者の操作どうしは、行為者の行の FOR NO KEY UPDATE で直列になる。
-- - 同じ2人の間の操作は、行為者がどちらでも、どちらかの行で FOR NO KEY UPDATE と FOR SHARE がぶつかるので直列になる。
-- モードの違う2行は1文ではロックできないので、isureal_lock_interaction_users と同じく2文に分ける。
CREATE OR REPLACE FUNCTION isureal_lock_actor_and_target(actor text, target text) RETURNS boolean
LANGUAGE plpgsql AS $$
DECLARE
  first_id text;
  second_id text;
BEGIN
  IF actor = target THEN
    RETURN false;
  END IF;
  IF actor COLLATE "C" < target COLLATE "C" THEN
    first_id := actor; second_id := target;
  ELSE
    first_id := target; second_id := actor;
  END IF;
  IF first_id = actor THEN
    PERFORM 1 FROM users u WHERE u.id = first_id AND u.status = 'active' FOR NO KEY UPDATE;
  ELSE
    PERFORM 1 FROM users u WHERE u.id = first_id AND u.status = 'active' FOR SHARE;
  END IF;
  IF NOT FOUND THEN
    RETURN false;
  END IF;
  IF second_id = actor THEN
    PERFORM 1 FROM users u WHERE u.id = second_id AND u.status = 'active' FOR NO KEY UPDATE;
  ELSE
    PERFORM 1 FROM users u WHERE u.id = second_id AND u.status = 'active' FOR SHARE;
  END IF;
  RETURN FOUND;
END $$;

CREATE OR REPLACE FUNCTION isureal_is_blocked(a text, b text) RETURNS boolean
LANGUAGE sql STABLE AS $$
  SELECT EXISTS(SELECT 1 FROM block_relations br WHERE (br.blocker_id = a AND br.blocked_id = b) OR (br.blocker_id = b AND br.blocked_id = a))
$$;

-- 友達申請。ブロック関係か友達関係があれば conflict。
-- 相手からの pending の申請があれば、それを承認して友達関係を作る。自分からの pending の申請があれば conflict。
-- どちらもなければ、新しい申請を作る。
-- 受信者の行は FOR SHARE でロックする（#87）。同じ受信者への別々の申請者からの申請は、並行して進む。
CREATE OR REPLACE FUNCTION isureal_create_friend_request(actor text, recipient text, new_request_id text, new_friendship_id text, low_id text, high_id text)
RETURNS TABLE(out_result text, out_id text, out_status text, out_updated_at timestamptz)
LANGUAGE plpgsql AS $$
DECLARE
  p friend_requests%ROWTYPE;
BEGIN
  IF NOT isureal_lock_actor_and_target(actor, recipient) THEN
    RETURN QUERY SELECT 'not_found'::text, NULL::text, NULL::text, NULL::timestamptz;
    RETURN;
  END IF;
  IF isureal_is_blocked(actor, recipient)
     OR EXISTS(SELECT 1 FROM friendships f WHERE (f.user_low_id = actor AND f.user_high_id = recipient) OR (f.user_low_id = recipient AND f.user_high_id = actor)) THEN
    RETURN QUERY SELECT 'conflict'::text, NULL::text, NULL::text, NULL::timestamptz;
    RETURN;
  END IF;
  SELECT * INTO p FROM friend_requests fr WHERE fr.status = 'pending'
    AND ((fr.requester_id = actor AND fr.recipient_id = recipient) OR (fr.requester_id = recipient AND fr.recipient_id = actor))
    LIMIT 1;
  IF FOUND THEN
    IF p.requester_id <> recipient THEN
      RETURN QUERY SELECT 'conflict'::text, NULL::text, NULL::text, NULL::timestamptz;
      RETURN;
    END IF;
    RETURN QUERY
      WITH accepted AS (
        UPDATE friend_requests fr SET status = 'accepted', updated_at = clock_timestamp()
        WHERE fr.id = p.id
        RETURNING fr.id, fr.status, fr.updated_at
      ), friendship AS (
        INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
        SELECT new_friendship_id, low_id, high_id, accepted.id FROM accepted
      )
      SELECT 'ok'::text, accepted.id::text, accepted.status, accepted.updated_at FROM accepted;
    RETURN;
  END IF;
  RETURN QUERY
    INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
    VALUES (new_request_id, actor, recipient, 'pending', clock_timestamp(), clock_timestamp())
    RETURNING 'ok'::text, friend_requests.id::text, friend_requests.status, friend_requests.updated_at;
END $$;

-- 友達申請の承認・拒否・取り下げ。申請がなければ not_found、操作する権限がなければ forbidden。
-- ロックの後、pending でなければ forbidden、承認でブロック関係があれば conflict。
-- 行為者でない側（承認・拒否なら申請者、取り下げなら受信者）の行は FOR SHARE でロックする（#87）。
CREATE OR REPLACE FUNCTION isureal_transition_friend_request(actor text, request_id text, new_status text, new_friendship_id text)
RETURNS TABLE(out_result text, out_id text, out_status text, out_updated_at timestamptz)
LANGUAGE plpgsql AS $$
DECLARE
  r friend_requests%ROWTYPE;
  current_status text;
BEGIN
  SELECT * INTO r FROM friend_requests fr WHERE fr.id = request_id;
  IF NOT FOUND THEN
    RETURN QUERY SELECT 'not_found'::text, NULL::text, NULL::text, NULL::timestamptz;
    RETURN;
  END IF;
  IF (new_status = 'cancelled' AND actor <> r.requester_id) OR (new_status <> 'cancelled' AND actor <> r.recipient_id) THEN
    RETURN QUERY SELECT 'forbidden'::text, NULL::text, NULL::text, NULL::timestamptz;
    RETURN;
  END IF;
  -- 上の確認で、行為者は申請者か受信者のどちらか。
  IF NOT isureal_lock_actor_and_target(actor, CASE WHEN actor = r.requester_id THEN r.recipient_id ELSE r.requester_id END) THEN
    RETURN QUERY SELECT 'not_found'::text, NULL::text, NULL::text, NULL::timestamptz;
    RETURN;
  END IF;
  -- 申請者と受信者は変わらないので、状態だけを読み直す
  SELECT fr.status INTO current_status FROM friend_requests fr WHERE fr.id = request_id;
  IF current_status <> 'pending' THEN
    RETURN QUERY SELECT 'forbidden'::text, NULL::text, NULL::text, NULL::timestamptz;
    RETURN;
  END IF;
  IF new_status = 'accepted' THEN
    IF isureal_is_blocked(r.requester_id, r.recipient_id) THEN
      RETURN QUERY SELECT 'conflict'::text, NULL::text, NULL::text, NULL::timestamptz;
      RETURN;
    END IF;
    INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
    VALUES (new_friendship_id,
            CASE WHEN r.requester_id COLLATE "C" < r.recipient_id COLLATE "C" THEN r.requester_id ELSE r.recipient_id END,
            CASE WHEN r.requester_id COLLATE "C" < r.recipient_id COLLATE "C" THEN r.recipient_id ELSE r.requester_id END,
            r.id);
  END IF;
  RETURN QUERY
    UPDATE friend_requests fr SET status = new_status, updated_at = clock_timestamp()
    WHERE fr.id = request_id
    RETURNING 'ok'::text, fr.id::text, fr.status, fr.updated_at;
END $$;

-- 友達の解除。友達でなければ conflict。
CREATE OR REPLACE FUNCTION isureal_delete_friendship(actor text, target text, low_id text, high_id text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT isureal_lock_user_pair(actor, target) THEN
    RETURN 'not_found';
  END IF;
  DELETE FROM friendships f WHERE f.user_low_id = low_id AND f.user_high_id = high_id;
  IF NOT FOUND THEN
    RETURN 'conflict';
  END IF;
  RETURN 'ok';
END $$;

-- フォロー。相手が認定公開アカウントでない、ブロック関係がある、すでにフォローしている場合は conflict。
-- 相手の行は FOR SHARE でロックする（#87）。同じ認定公開アカウントへの別々の利用者からのフォローは、並行して進む。
CREATE OR REPLACE FUNCTION isureal_follow(actor text, target text, new_follow_id text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT isureal_lock_actor_and_target(actor, target) THEN
    RETURN 'not_found';
  END IF;
  INSERT INTO follows (id, follower_id, followee_id)
  SELECT new_follow_id, actor, target FROM users u
  WHERE u.id = target AND u.status = 'active' AND u.account_type = 'certified_public'
    AND NOT isureal_is_blocked(actor, target)
    AND NOT EXISTS(SELECT 1 FROM follows fo WHERE fo.follower_id = actor AND fo.followee_id = target);
  IF NOT FOUND THEN
    RETURN 'conflict';
  END IF;
  RETURN 'ok';
END $$;

-- フォローの解除。フォローしていなければ conflict。相手の行は FOR SHARE でロックする（#87）。
CREATE OR REPLACE FUNCTION isureal_unfollow(actor text, target text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT isureal_lock_actor_and_target(actor, target) THEN
    RETURN 'not_found';
  END IF;
  DELETE FROM follows fo WHERE fo.follower_id = actor AND fo.followee_id = target;
  IF NOT FOUND THEN
    RETURN 'conflict';
  END IF;
  RETURN 'ok';
END $$;

-- ブロック。すでにブロックしていれば conflict。
-- 新しくブロックしたときだけ、友達関係・双方向のフォロー・pending の申請を後始末する。
CREATE OR REPLACE FUNCTION isureal_block(actor text, target text, new_block_id text, low_id text, high_id text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT isureal_lock_user_pair(actor, target) THEN
    RETURN 'not_found';
  END IF;
  INSERT INTO block_relations (id, blocker_id, blocked_id)
  SELECT new_block_id, actor, target
  WHERE NOT EXISTS(SELECT 1 FROM block_relations br WHERE br.blocker_id = actor AND br.blocked_id = target);
  IF NOT FOUND THEN
    RETURN 'conflict';
  END IF;
  DELETE FROM friendships f WHERE f.user_low_id = low_id AND f.user_high_id = high_id;
  DELETE FROM follows fo WHERE (fo.follower_id = actor AND fo.followee_id = target) OR (fo.follower_id = target AND fo.followee_id = actor);
  UPDATE friend_requests fr SET status = 'cancelled', updated_at = clock_timestamp()
  WHERE fr.status = 'pending' AND ((fr.requester_id = actor AND fr.recipient_id = target) OR (fr.requester_id = target AND fr.recipient_id = actor));
  RETURN 'ok';
END $$;

-- ブロックの解除。ブロックしていなければ forbidden。
CREATE OR REPLACE FUNCTION isureal_unblock(actor text, target text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT isureal_lock_user_pair(actor, target) THEN
    RETURN 'not_found';
  END IF;
  DELETE FROM block_relations br WHERE br.blocker_id = actor AND br.blocked_id = target;
  IF NOT FOUND THEN
    RETURN 'forbidden';
  END IF;
  RETURN 'ok';
END $$;

-- 利用者のアカウントに関わる書き込みを、ロックの取得から確定までを DB の中で行う関数にする（#67）。
-- アプリは autocommit の1文で呼ぶので、europe と america から DB への往復は1回になり、
-- users の行のロックを持つのは関数の実行中だけになる。
-- 関数の中の文はそれぞれ新しいスナップショットで動くので、ロックの後の文は、変更前の Go のトランザクションと同じく、ロック後の状態を見る。
-- （CTE の1文にすると、ロックを待ったあとも文の開始時点のスナップショットで login_sessions を読むので、
--   同時のログインで追加されたセッションを退会で無効にし損なう。）

-- ログイン（loginHandler）。out_result: ok / invalid_credentials（401）。
-- 変更前と同じ順序：ユーザー名で行をロック → active と SecretKey の確認 → 有効なセッションを無効にして、新しいセッションを追加。
-- 追加するセッションは無効にする文の後に INSERT するので、無効にはならない。
CREATE OR REPLACE FUNCTION isureal_login(p_username text, p_secret_key_hash text, p_session_id text, p_token_hash text)
RETURNS TABLE(out_result text,
              out_user_id text, out_username text, out_secret_key_hash text, out_display_name text, out_bio text,
              out_profile_image_id text, out_region text, out_account_type text, out_status text)
LANGUAGE plpgsql AS $$
DECLARE
  u users%ROWTYPE;
BEGIN
  SELECT * INTO u FROM users WHERE username = p_username FOR NO KEY UPDATE;
  IF NOT FOUND OR u.status <> 'active' OR u.secret_key_hash <> p_secret_key_hash THEN
    RETURN QUERY SELECT 'invalid_credentials'::text,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  UPDATE login_sessions SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
  WHERE user_id = u.id AND active = true;
  INSERT INTO login_sessions (id, user_id, token_hash) VALUES (p_session_id, u.id, p_token_hash);
  RETURN QUERY SELECT 'ok'::text,
    u.id::text, u.username::text, u.secret_key_hash, u.display_name, u.bio, u.profile_image_id::text, u.region, u.account_type, u.status;
END $$;

-- プロフィール画像の設定（putProfileImageHandler）。利用者の行がなければ not_found（404 account_not_found）。
-- 変更前と同じ順序：本人の行をロック → 画像の INSERT → profile_image_id の更新 → 前の画像の DELETE。
-- 前の画像の id はロックの後に読むので、同じ利用者の同時の差し替えでも、最後に設定された画像の行だけが残る。
CREATE OR REPLACE FUNCTION isureal_put_profile_image(p_user_id text, p_image_id text, p_image bytea) RETURNS text
LANGUAGE plpgsql AS $$
DECLARE
  old_image_id text;
BEGIN
  SELECT u.profile_image_id INTO old_image_id FROM users u WHERE u.id = p_user_id FOR NO KEY UPDATE;
  IF NOT FOUND THEN
    RETURN 'not_found';
  END IF;
  INSERT INTO image_objects (id, owner_user_id, data, media_type) VALUES (p_image_id, p_user_id, p_image, 'image/jpeg');
  UPDATE users SET profile_image_id = p_image_id, updated_at = clock_timestamp() WHERE id = p_user_id;
  IF old_image_id IS NOT NULL THEN
    DELETE FROM image_objects WHERE id = old_image_id;
  END IF;
  RETURN 'ok';
END $$;

-- プロフィール画像の削除（deleteProfileImageHandler）。利用者の行がないか、画像が設定されていなければ not_found（404 account_not_found）。
CREATE OR REPLACE FUNCTION isureal_delete_profile_image(p_user_id text) RETURNS text
LANGUAGE plpgsql AS $$
DECLARE
  old_image_id text;
BEGIN
  SELECT u.profile_image_id INTO old_image_id FROM users u WHERE u.id = p_user_id FOR NO KEY UPDATE;
  IF NOT FOUND OR old_image_id IS NULL THEN
    RETURN 'not_found';
  END IF;
  UPDATE users SET profile_image_id = NULL, updated_at = clock_timestamp() WHERE id = p_user_id;
  DELETE FROM image_objects WHERE id = old_image_id;
  RETURN 'ok';
END $$;

-- 退会（deleteAccountHandler）。active な利用者がいなければ not_found（404 account_not_found）。
-- 本人の行をロックしてから、状態の更新と、有効なセッションの無効化を行う。
CREATE OR REPLACE FUNCTION isureal_delete_account(p_user_id text) RETURNS text
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM 1 FROM users u WHERE u.id = p_user_id AND u.status = 'active' FOR NO KEY UPDATE;
  IF NOT FOUND THEN
    RETURN 'not_found';
  END IF;
  UPDATE users SET status = 'deleted', deleted_at = clock_timestamp(), updated_at = clock_timestamp() WHERE id = p_user_id;
  UPDATE login_sessions SET active = false, invalidated_at = clock_timestamp(), reason = 'account_deletion'
  WHERE user_id = p_user_id AND active = true;
  RETURN 'ok';
END $$;
