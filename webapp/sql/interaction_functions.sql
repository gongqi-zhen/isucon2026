-- 投稿・RealMoji・コメントを、ロックの取得から確定までを DB の中で行う関数（#50）。
-- アプリは autocommit の1文で呼ぶので、europe と america から DB への往復は1回になり、
-- users の行や投稿期間の行のロックを持つのは関数の実行中だけになる。
-- 関数の中の文は、READ COMMITTED で文ごとに新しいスナップショットを使う。
-- そのため、ロックを待ったあとに件数や権限を確かめる、という変更前の順序をそのまま保てる。
-- init.sh が migrate.sql の後に毎回流す（CREATE OR REPLACE なので何度流してもよい）。

-- loadPostAccess（post.go）と同じ判定材料を返す。投稿がない、または作者が active でなければ0行。
CREATE OR REPLACE FUNCTION isureal_post_access(viewer_id text, target_post_id text)
RETURNS TABLE(author_id text, visibility text, window_ended_at timestamptz,
              viewer_active boolean, viewer_has_posted_today boolean, blocked boolean, friend boolean, friend_of_friend boolean)
LANGUAGE sql STABLE AS $$
  WITH viewer AS (SELECT u.id, u.region FROM users u WHERE u.id = viewer_id AND u.status = 'active'),
  viewer_friends AS (
    SELECT CASE WHEN f.user_low_id = viewer_id THEN f.user_high_id ELSE f.user_low_id END AS id
    FROM friendships f WHERE f.user_low_id = viewer_id OR f.user_high_id = viewer_id
  )
  SELECT p.author_id, p.visibility, w.ended_at,
    EXISTS(SELECT 1 FROM viewer),
    EXISTS(
      SELECT 1 FROM posts vp
      JOIN viewer ON viewer.id = vp.author_id
      JOIN posting_windows posted ON posted.id = vp.posting_window_id
      JOIN posting_windows current ON current.region = viewer.region AND current.ended_at IS NULL
                                  AND current.local_date = posted.local_date
      WHERE vp.deleted_at IS NULL
    ),
    EXISTS(
      SELECT 1 FROM block_relations b
      WHERE (b.blocker_id = viewer_id AND b.blocked_id = p.author_id) OR (b.blocker_id = p.author_id AND b.blocked_id = viewer_id)
    ),
    EXISTS(SELECT 1 FROM viewer_friends vf WHERE vf.id = p.author_id),
    EXISTS(
      SELECT 1 FROM friendships af JOIN viewer_friends vf
        ON (af.user_low_id = vf.id AND af.user_high_id = p.author_id)
        OR (af.user_high_id = vf.id AND af.user_low_id = p.author_id)
    )
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.deleted_at IS NULL AND p.id = target_post_id
$$;

-- postAccess.canView（post.go の viewerContext.canView）と同じ判定。投稿がなければ false。
CREATE OR REPLACE FUNCTION isureal_can_view(viewer_id text, target_post_id text) RETURNS boolean
LANGUAGE plpgsql STABLE AS $$
DECLARE
  a record;
BEGIN
  SELECT * INTO a FROM isureal_post_access(viewer_id, target_post_id);
  IF NOT FOUND OR NOT a.viewer_active THEN
    RETURN false;
  END IF;
  IF a.author_id = viewer_id THEN
    RETURN true;
  END IF;
  IF a.window_ended_at IS NOT NULL OR NOT a.viewer_has_posted_today OR a.blocked THEN
    RETURN false;
  END IF;
  RETURN CASE a.visibility
    WHEN 'public' THEN true
    WHEN 'friends' THEN a.friend
    WHEN 'friends_of_friends' THEN a.friend OR a.friend_of_friend
    ELSE false
  END;
END $$;

-- canComment（comment.go）と同じ判定。投稿がなければ false。
CREATE OR REPLACE FUNCTION isureal_can_comment(viewer_id text, target_post_id text) RETURNS boolean
LANGUAGE plpgsql STABLE AS $$
DECLARE
  a record;
BEGIN
  SELECT * INTO a FROM isureal_post_access(viewer_id, target_post_id);
  IF NOT FOUND OR NOT a.viewer_active THEN
    RETURN false;
  END IF;
  IF NOT isureal_can_view(viewer_id, target_post_id) OR a.window_ended_at IS NOT NULL THEN
    RETURN false;
  END IF;
  IF a.author_id = viewer_id THEN
    RETURN a.viewer_has_posted_today;
  END IF;
  IF a.visibility = 'public' THEN
    RETURN true;
  END IF;
  RETURN a.friend;
END $$;

-- lockInteractionUsers（user.go）と同じロック。行為者を FOR NO KEY UPDATE、投稿者を FOR SHARE で、id の昇順（COLLATE "C"）にロックする。
-- 行為者と投稿者が同じなら、その1行を FOR NO KEY UPDATE でロックする。どちらかがなければ false。
CREATE OR REPLACE FUNCTION isureal_lock_interaction_users(actor text, author text) RETURNS boolean
LANGUAGE plpgsql AS $$
DECLARE
  first_id text;
  second_id text;
BEGIN
  IF actor = author THEN
    PERFORM 1 FROM users WHERE id = actor FOR NO KEY UPDATE;
    RETURN FOUND;
  END IF;
  IF actor COLLATE "C" < author COLLATE "C" THEN
    first_id := actor; second_id := author;
  ELSE
    first_id := author; second_id := actor;
  END IF;
  IF first_id = actor THEN
    PERFORM 1 FROM users WHERE id = first_id FOR NO KEY UPDATE;
  ELSE
    PERFORM 1 FROM users WHERE id = first_id FOR SHARE;
  END IF;
  IF NOT FOUND THEN
    RETURN false;
  END IF;
  IF second_id = actor THEN
    PERFORM 1 FROM users WHERE id = second_id FOR NO KEY UPDATE;
  ELSE
    PERFORM 1 FROM users WHERE id = second_id FOR SHARE;
  END IF;
  RETURN FOUND;
END $$;

-- 投稿（createPostHandler）。
-- out_result: ok / slot_unavailable（409）/ user_not_found（変更前と同じく 500）。
-- 変更前と同じ順序：本人の行をロック → 投稿期間を FOR SHARE（終了と入れ違いなら1回取り直す）→ その日の件数 → 投稿。
-- 画像は、受け付けると決まってから INSERT する（変更前は先に INSERT して、受け付けなければロールバックしていた）。
-- 画像の created_at と投稿の posted_at には同じ時刻を入れる（#40 のパーティションの絞り込みが効く）。
CREATE OR REPLACE FUNCTION isureal_create_post(
  p_user_id text, p_post_id text, p_image_id text, p_image bytea,
  p_caption text, p_visibility text, p_location_kind text, p_latitude numeric, p_longitude numeric, p_locality text)
RETURNS TABLE(out_result text, out_posting_window_id text, out_posted_at timestamptz, out_on_time_deadline timestamptz,
              out_latitude numeric, out_longitude numeric,
              out_user_id text, out_username text, out_secret_key_hash text, out_display_name text, out_bio text,
              out_profile_image_id text, out_region text, out_account_type text, out_status text)
LANGUAGE plpgsql AS $$
DECLARE
  u users%ROWTYPE;
  w posting_windows%ROWTYPE;
  n_active int;
  n_deleted int;
  t timestamptz;
BEGIN
  SELECT * INTO u FROM users WHERE id = p_user_id FOR NO KEY UPDATE;
  IF NOT FOUND OR u.status <> 'active' THEN
    RETURN QUERY SELECT 'user_not_found'::text, NULL::text, NULL::timestamptz, NULL::timestamptz, NULL::numeric, NULL::numeric,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  -- 投稿期間の終了と入れ違いになると、ロックを待ったあとの再評価で行が条件に合わなくなる。
  -- 次の文は新しいスナップショットを使うので、新しい投稿期間が見える（変更前の取り直しと同じ）。
  SELECT * INTO w FROM posting_windows WHERE region = u.region AND ended_at IS NULL FOR SHARE;
  IF NOT FOUND THEN
    SELECT * INTO w FROM posting_windows WHERE region = u.region AND ended_at IS NULL FOR SHARE;
  END IF;
  IF FOUND THEN
    SELECT count(*) FILTER (WHERE p.deleted_at IS NULL), count(*) FILTER (WHERE p.deleted_at IS NOT NULL)
      INTO n_active, n_deleted
      FROM posts p JOIN posting_windows pw ON pw.id = p.posting_window_id
      WHERE p.author_id = p_user_id AND pw.local_date = w.local_date;
    t := clock_timestamp();
  END IF;
  IF NOT FOUND OR n_active <> 0 OR n_deleted >= 2 OR t < w.starts_at THEN
    RETURN QUERY SELECT 'slot_unavailable'::text, NULL::text, NULL::timestamptz, NULL::timestamptz, NULL::numeric, NULL::numeric,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES (p_image_id, p_user_id, p_image, 'image/jpeg', t);
  INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, latitude, longitude, locality, posted_at)
    VALUES (p_post_id, p_user_id, w.id, p_image_id, p_caption, p_visibility, p_location_kind, p_latitude, p_longitude, p_locality, t);
  RETURN QUERY SELECT 'ok'::text, w.id::text, t, w.on_time_deadline, p.latitude, p.longitude,
    u.id::text, u.username::text, u.secret_key_hash, u.display_name, u.bio, u.profile_image_id::text, u.region, u.account_type, u.status
    FROM posts p WHERE p.id = p_post_id;
END $$;

-- RealMoji（replaceRealMojiHandler）。
-- out_result: ok / forbidden（403 interaction_forbidden）/ not_found（404 relationship_not_found）。
-- 変更前と同じ順序：投稿の確認 → 自分の投稿なら forbidden → 2人をロック → active の確認 → 閲覧権限 → 置き換え。
CREATE OR REPLACE FUNCTION isureal_create_realmoji(
  p_actor text, p_post_id text, p_realmoji_id text, p_image_id text, p_image bytea, p_reaction_type text)
RETURNS TABLE(out_result text, out_id text, out_post_id text, out_reactor_id text, out_image_id text, out_reaction_type text, out_created_at timestamptz,
              out_user_id text, out_username text, out_secret_key_hash text, out_display_name text, out_bio text,
              out_profile_image_id text, out_region text, out_account_type text, out_status text)
LANGUAGE plpgsql AS $$
DECLARE
  post_author text;
  actor_user users%ROWTYPE;
  author_status text;
  r realmojis%ROWTYPE;
BEGIN
  SELECT p.author_id INTO post_author FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
    WHERE p.id = p_post_id AND p.deleted_at IS NULL;
  IF NOT FOUND OR post_author = p_actor THEN
    RETURN QUERY SELECT 'forbidden'::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::timestamptz,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  IF NOT isureal_lock_interaction_users(p_actor, post_author) THEN
    RETURN QUERY SELECT 'not_found'::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::timestamptz,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  SELECT * INTO actor_user FROM users WHERE id = p_actor;
  SELECT status INTO author_status FROM users WHERE id = post_author;
  IF actor_user.status <> 'active' OR author_status <> 'active' THEN
    RETURN QUERY SELECT 'not_found'::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::timestamptz,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  IF NOT isureal_can_view(p_actor, p_post_id) THEN
    RETURN QUERY SELECT 'forbidden'::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::timestamptz,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  INSERT INTO image_objects (id, owner_user_id, data, media_type) VALUES (p_image_id, p_actor, p_image, 'image/jpeg');
  UPDATE realmojis SET replaced_at = clock_timestamp()
    WHERE post_id = p_post_id AND reactor_id = p_actor AND replaced_at IS NULL AND deleted_at IS NULL;
  INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at)
    VALUES (p_realmoji_id, p_post_id, p_actor, p_image_id, p_reaction_type, clock_timestamp())
    RETURNING * INTO r;
  RETURN QUERY SELECT 'ok'::text, r.id::text, r.post_id::text, r.reactor_id::text, r.image_id::text, r.reaction_type, r.created_at,
    actor_user.id::text, actor_user.username::text, actor_user.secret_key_hash, actor_user.display_name, actor_user.bio,
    actor_user.profile_image_id::text, actor_user.region, actor_user.account_type, actor_user.status;
END $$;

-- コメント（createCommentHandler）。
-- out_result: ok / forbidden（403 interaction_forbidden）/ user_not_found（変更前と同じく 500）。
-- 変更前と同じ順序：投稿の確認 → 2人（自分の投稿なら1人）をロック → コメントの権限 → 追加。
CREATE OR REPLACE FUNCTION isureal_create_comment(p_actor text, p_post_id text, p_comment_id text, p_body text)
RETURNS TABLE(out_result text, out_id text, out_post_id text, out_author_id text, out_body text, out_created_at timestamptz,
              out_user_id text, out_username text, out_secret_key_hash text, out_display_name text, out_bio text,
              out_profile_image_id text, out_region text, out_account_type text, out_status text)
LANGUAGE plpgsql AS $$
DECLARE
  post_author text;
  actor_user users%ROWTYPE;
  c comments%ROWTYPE;
BEGIN
  SELECT p.author_id INTO post_author FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
    WHERE p.id = p_post_id AND p.deleted_at IS NULL;
  IF NOT FOUND THEN
    RETURN QUERY SELECT 'forbidden'::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::timestamptz,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  IF NOT isureal_lock_interaction_users(p_actor, post_author) THEN
    RETURN QUERY SELECT 'user_not_found'::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::timestamptz,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  IF NOT isureal_can_comment(p_actor, p_post_id) THEN
    RETURN QUERY SELECT 'forbidden'::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::timestamptz,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  SELECT * INTO actor_user FROM users WHERE id = p_actor;
  IF actor_user.status <> 'active' THEN
    RETURN QUERY SELECT 'user_not_found'::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::timestamptz,
      NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text;
    RETURN;
  END IF;
  INSERT INTO comments (id, post_id, author_id, body, created_at) VALUES (p_comment_id, p_post_id, p_actor, p_body, clock_timestamp())
    RETURNING * INTO c;
  RETURN QUERY SELECT 'ok'::text, c.id::text, c.post_id::text, c.author_id::text, c.body, c.created_at,
    actor_user.id::text, actor_user.username::text, actor_user.secret_key_hash, actor_user.display_name, actor_user.bio,
    actor_user.profile_image_id::text, actor_user.region, actor_user.account_type, actor_user.status;
END $$;
