-- 投稿期間の開始（openPostingWindowHandler）を、ロックの取得から確定までを DB の中で行う関数（#72）。
-- アプリは autocommit の1文で呼ぶので、europe と america から DB への往復は1回になる。
-- 関数の中の文は、READ COMMITTED で文ごとに新しいスナップショットを使う。
-- そのため、advisory lock を待ったあとの重複の確認には、先に開いた投稿期間が見える（変更前と同じ）。
-- init.sh が migrate.sql の後に毎回流す（CREATE OR REPLACE なので何度流してもよい）。

-- out_result: ok / conflict（409 posting_window_conflict）。
-- 順序：同じリージョンの開始を advisory lock で直列にする → 重複の確認 → DB の時計で開始時刻を決める
-- → 通知を一括挿入する（#41）→ 前の投稿期間を終える → 投稿期間を挿入する。
-- 前の投稿期間を終える UPDATE は、その行を確定までロックし、そのリージョンの投稿（FOR SHARE）を待たせる。
-- 通知の一括挿入はその行のロックが要らない（通知には外部キーがなく、確定までは見えない）ので、UPDATE より先に行う（#85）。
-- pg_notify（#60）は、重複がないと決まってから送る。conflict でも関数は正常に終わり確定するので、先に送ると届いてしまう。
CREATE OR REPLACE FUNCTION isureal_open_posting_window(
  p_region text, p_local_date date, p_on_time_seconds bigint, p_window_id text, p_notify_channel text)
RETURNS TABLE(out_result text, out_id text, out_region text, out_local_date date,
              out_starts_at timestamptz, out_on_time_deadline timestamptz)
LANGUAGE plpgsql AS $$
DECLARE
  t timestamptz;
BEGIN
  -- 代案（#111、有効にしていない）：synchronous_commit = off を DB 全体（etc/postgresql/conf.d/isucon.conf、#100）
  -- ではなく、この関数のトランザクションだけに効かせる。通知の一括挿入（そのリージョンの全利用者分）で WAL が
  -- いちばん大きくなるのはこの関数なので、ここだけ COMMIT の fsync を待たなければ、前の投稿期間の行のロックを
  -- 持つ時間と、admin の応答を短くできる。ほかの書き込みは fsync を待つままなので、クラッシュで失われうるのは
  -- 投稿期間の開始だけになる。
  -- 3つ目の引数 true は SET LOCAL と同じで、トランザクションが終わると元に戻る。COMMIT はトランザクションの
  -- 一部なので、この設定のまま行われる（ALTER FUNCTION ... SET は関数を抜けるときに戻るので効かない）。
  -- DB 全体を off にしているあいだは意味がないので、isucon.conf の設定を戻すときに、代わりにこちらを有効にするかを決める。
  -- PERFORM set_config('synchronous_commit', 'off', true);

  -- users のテーブルロックは登録やプロフィール更新まで止めてしまうので使わない。
  PERFORM pg_advisory_xact_lock(hashtext('posting_window:' || p_region));
  IF EXISTS(SELECT 1 FROM posting_windows WHERE region = p_region AND local_date >= p_local_date) THEN
    RETURN QUERY SELECT 'conflict'::text, NULL::text, NULL::text, NULL::date, NULL::timestamptz, NULL::timestamptz;
    RETURN;
  END IF;
  -- 各プロセスの通知ハブにすぐ確認させる（#60）。届くのは確定した後だけ。
  PERFORM pg_notify(p_notify_channel, p_region);
  t := clock_timestamp();
  -- ID は先頭48ビットに開始時刻（ミリ秒）を入れた UUID v7 にする。ランダムな UUID だと、主キーのインデックスの
  -- ばらばらのページに1行ずつ書き込むことになり、利用者の数だけページを読み書きする（#41）。
  -- 同じ文で挿入する行は先頭が同じになり、インデックスの近いページにまとまる。
  INSERT INTO notifications (id, user_id, posting_window_id, created_at)
  SELECT encode(set_bit(set_bit(overlay(uuid_send(gen_random_uuid())
           PLACING substring(int8send(floor(extract(epoch FROM t) * 1000)::bigint) FROM 3) FROM 1 FOR 6),
           52, 1), 53, 1), 'hex')::uuid::text,
         u.id, p_window_id, t
  FROM users u WHERE u.region = p_region AND u.status = 'active';
  UPDATE posting_windows SET ended_at = t WHERE region = p_region AND ended_at IS NULL;
  -- 開いている投稿期間は1つだけ（posting_windows_one_active_region）なので、前の投稿期間を終えてから挿入する。
  INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
    VALUES (p_window_id, p_region, p_local_date, t, t + p_on_time_seconds * interval '1 second');
  RETURN QUERY SELECT 'ok'::text, w.id::text, w.region, w.local_date, w.starts_at, w.on_time_deadline
    FROM posting_windows w WHERE w.id = p_window_id;
END $$;
