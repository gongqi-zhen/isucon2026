-- 画像の有効性の変化を、各プロセスがまとめて検知するための記録（#64）。
-- init.sh が reset.sql の後に毎回流す。何度流してもよい。
--
-- 画像を無効にする変更（投稿の削除、RealMoji の置き換え・削除、プロフィール画像の変更・削除、退会）を、
-- トリガで image_invalidations に1行ずつ記録する。行は次のどれか1つを持つ。
--   image_id：その画像が無効になった
--   user_id ：その利用者が持ち主の画像と、その利用者の投稿への RealMoji の画像が無効になった
--   post_id ：その投稿の画像と、その投稿への RealMoji の画像が無効になった
--
-- 各プロセスの1本の goroutine（webapp/go/image_validity.go）が、前回読んだときのスナップショットの xmin 以上の
-- xid の行を読む。xmin より小さい xid のトランザクションは前回の読み込みの時点で終わっていたので、
-- コミットしていれば前回読めている。それ以外（前回の時点で実行中か、その後に始まったもの）は今回の条件に入る。
-- そのため、時刻の余裕に頼らずに、コミットした無効化を取りこぼさない。
--
-- reset.sql は session_replication_role = replica で流すので、初期化の変更ではトリガは動かない。
-- 代わりに、最後に記録を空にして世代（epoch）を変え、各プロセスに有効性のキャッシュを捨てさせる。

CREATE TABLE IF NOT EXISTS image_invalidations (
  xid xid8 NOT NULL DEFAULT pg_current_xact_id(),
  image_id text,
  user_id text,
  post_id text
);
CREATE INDEX IF NOT EXISTS image_invalidations_xid ON image_invalidations(xid);

CREATE TABLE IF NOT EXISTS image_invalidation_epoch (
  id int PRIMARY KEY CHECK (id = 1),
  epoch text NOT NULL
);

-- 投稿の削除。その投稿の画像と、その投稿への RealMoji の画像が無効になる。
CREATE OR REPLACE FUNCTION isureal_invalidate_post_images() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO image_invalidations (post_id) VALUES (OLD.id);
  RETURN NULL;
END $$;

CREATE OR REPLACE TRIGGER posts_invalidate_images
AFTER UPDATE ON posts FOR EACH ROW
WHEN ((OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL)
   OR OLD.image_id IS DISTINCT FROM NEW.image_id OR OLD.author_id IS DISTINCT FROM NEW.author_id)
EXECUTE FUNCTION isureal_invalidate_post_images();

CREATE OR REPLACE TRIGGER posts_invalidate_images_on_delete
AFTER DELETE ON posts FOR EACH ROW
EXECUTE FUNCTION isureal_invalidate_post_images();

-- RealMoji の置き換えと削除。
CREATE OR REPLACE FUNCTION isureal_invalidate_realmoji_image() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO image_invalidations (image_id) VALUES (OLD.image_id);
  RETURN NULL;
END $$;

CREATE OR REPLACE TRIGGER realmojis_invalidate_image
AFTER UPDATE ON realmojis FOR EACH ROW
WHEN ((OLD.replaced_at IS NULL AND NEW.replaced_at IS NOT NULL)
   OR (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL)
   OR OLD.image_id IS DISTINCT FROM NEW.image_id OR OLD.post_id IS DISTINCT FROM NEW.post_id
   OR OLD.reactor_id IS DISTINCT FROM NEW.reactor_id)
EXECUTE FUNCTION isureal_invalidate_realmoji_image();

CREATE OR REPLACE TRIGGER realmojis_invalidate_image_on_delete
AFTER DELETE ON realmojis FOR EACH ROW
EXECUTE FUNCTION isureal_invalidate_realmoji_image();

-- 退会（active でなくなる）と、プロフィール画像の変更・削除。
CREATE OR REPLACE FUNCTION isureal_invalidate_user_images() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    INSERT INTO image_invalidations (user_id) VALUES (OLD.id);
    RETURN NULL;
  END IF;
  IF OLD.status = 'active' AND NEW.status IS DISTINCT FROM 'active' THEN
    INSERT INTO image_invalidations (user_id) VALUES (OLD.id);
  END IF;
  IF OLD.profile_image_id IS NOT NULL AND OLD.profile_image_id IS DISTINCT FROM NEW.profile_image_id THEN
    INSERT INTO image_invalidations (image_id) VALUES (OLD.profile_image_id);
  END IF;
  RETURN NULL;
END $$;

CREATE OR REPLACE TRIGGER users_invalidate_images
AFTER UPDATE ON users FOR EACH ROW
WHEN ((OLD.status = 'active' AND NEW.status IS DISTINCT FROM 'active')
   OR (OLD.profile_image_id IS NOT NULL AND OLD.profile_image_id IS DISTINCT FROM NEW.profile_image_id))
EXECUTE FUNCTION isureal_invalidate_user_images();

CREATE OR REPLACE TRIGGER users_invalidate_images_on_delete
AFTER DELETE ON users FOR EACH ROW
EXECUTE FUNCTION isureal_invalidate_user_images();

-- 画像の行の削除（プロフィール画像の差し替えで古い画像を消す）。
CREATE OR REPLACE FUNCTION isureal_invalidate_image_object() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO image_invalidations (image_id) VALUES (OLD.id);
  RETURN NULL;
END $$;

CREATE OR REPLACE TRIGGER image_objects_invalidate_on_delete
AFTER DELETE ON image_objects FOR EACH ROW
EXECUTE FUNCTION isureal_invalidate_image_object();

-- 初期化：記録を空にして、世代を変える。同じトランザクションで行い、空にしたのに世代が古いままの状態を見せない。
BEGIN;
TRUNCATE image_invalidations;
INSERT INTO image_invalidation_epoch (id, epoch) VALUES (1, gen_random_uuid()::text)
ON CONFLICT (id) DO UPDATE SET epoch = EXCLUDED.epoch;
COMMIT;
