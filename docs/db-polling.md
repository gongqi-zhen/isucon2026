# バックグラウンドの DB ポーリングの負荷（#110）

各プロセス（asia1、europe1、america1）は、リクエストとは別に DB を定期的に読んでいます。
この文書は、その負荷を `pg_stat_statements` で測る手順と、#110 で入れた2つの対策、変更の前後で比べる手順をまとめたものです。

## 1. 何をポーリングしているか

| 何を | どこで | 間隔 | 1回の大きさ |
|---|---|---|---|
| セッションの確かめ直し | `session_cache.go` `revalidate` | 100ms | 持っているトークンを `unnest` で渡し、`login_sessions` と `users` を引いて、利用者の行ごとに `md5(row(...))` を計算する |
| SSE の利用者の確認（退会・リージョンの変更） | `notification_hub.go` `checkUsers` | **500ms → 5秒**（#110） | 待機中の全接続の利用者を `id = ANY($1)` で引く |
| 開いている投稿期間 | `notification_hub.go` `checkWindows` | 100ms | 1行。LISTEN/NOTIFY（#60）の保険 |
| IP BAN の一覧 | `ip_ban.go` | 200ms | `ip_bans` 全件 |
| 画像の無効化 | `image_validity.go` | 500ms | `image_invalidations` の差分 |

## 2. #110 で入れた対策

### 2-1. セッションの確かめ直しは、最近使ったトークンだけ
- エントリに、最後に `get` で引かれた（または `put` で入った）時刻 `lastUsedAt` を持つ。
- 確かめ直しの対象は、最後に使ってから `ISUREAL_SESSION_CACHE_IDLE_MS`（既定 5000）以内のものだけ。それより古いエントリは、確かめ直さずに消す。
- 消したトークンが次に使われたときは、DB を引き直して（europe と america では1往復 約100ms）、`put` で入り直す。
- `session_cache.go` 冒頭の約束1〜3はそのまま成り立つ。消すだけで、validatedAt を進めない結果を使うことはない。
- 走行中に使われ続けるトークンの数は、`login_sessions` の有効な行の数ではなく、直近5秒に API を呼んだ利用者の数になる。

### 2-2. SSE の利用者の確認は LISTEN/NOTIFY
- `migrate.sql` のトリガ `users_notify_change`（`status`、`region` の変更）と `users_notify_change_on_delete` が、チャンネル `isureal_users` に利用者の id を `pg_notify` する。COMMIT のときだけ届く。
- 各プロセスの LISTEN する接続（投稿期間と同じ1本。`LISTEN isureal_posting_windows; LISTEN isureal_users`）が受け取り、その利用者の接続だけを、以前と同じ判定（`ActiveUserRegions`）で確かめて閉じる。
- ポーリング（`checkUsers`）は、LISTEN の接続が切れている間と、トリガが動かない初期化（`reset.sql` は `session_replication_role = replica` で流す）の取りこぼしの保険として、間隔を 500ms から 5秒に伸ばして残す。
  - 初期化で状態が戻った利用者の SSE 接続は、最大 5秒遅れて閉じる。初期化の直後の走行では、ベンチマーカーが接続をつなぎ直すので影響はない。

## 3. 測り方（asia2 で、読み取りだけ）

走行の前にリセットする。

```sql
SELECT pg_stat_statements_reset();
```

走行の後に、ポーリングのクエリを集計する。`pg_stat_statements.track = all` にしておくと、関数の中の文も別の行として出る（既定の設定では `none` にしてあるので、測る前に `all` にして `pg_reload_conf()` する。`etc/postgresql/conf.d/isucon.conf`）。

```sql
SELECT calls, round(total_exec_time) AS total_ms, round(mean_exec_time::numeric, 2) AS mean_ms, rows,
       left(regexp_replace(query, '\s+', ' ', 'g'), 100) AS query
FROM pg_stat_statements
WHERE query LIKE '%unnest($1::text[], $2::text[])%'                 -- セッションの確かめ直し
   OR query LIKE '%FROM users WHERE id = ANY($1) AND status%'       -- SSE の利用者の確認
   OR query LIKE '%FROM ip_bans WHERE%'                             -- IP BAN
   OR query LIKE '%image_invalidations%'                            -- 画像の無効化
   OR query LIKE '%FROM posting_windows WHERE ended_at IS NULL ORDER BY id%'  -- 投稿期間
ORDER BY total_exec_time DESC;
```

走行全体に占める割合を出す。

```sql
SELECT round(sum(total_exec_time)) AS total_ms, sum(calls) AS calls FROM pg_stat_statements;
```

セッションの確かめ直しについては、`rows / calls` が「1回に確かめているトークンの数」になる。対策 2-1 の後は、`login_sessions` の有効な行の数より小さくなるはず。

```sql
SELECT count(*) FILTER (WHERE u.region = 'europe') AS europe, count(*) FILTER (WHERE u.region = 'america') AS america
FROM login_sessions s JOIN users u ON u.id = s.user_id WHERE s.active;
```

## 4. 変更の前後で比べる

1. 変更前の版で走行し、3章の集計を記録する（`total_ms`、`calls`、`rows` と、全体に占める割合）。
2. #21 の手順で3台に配り、`webapp/sql/migrate.sql` も asia2 に配る。トリガは `init.sh` が流す `migrate.sql` で作られるので、初期化を1回行えば入る（手で入れるなら `psql -f webapp/sql/migrate.sql`）。
3. 走行し、同じ集計を記録する。
   - セッションの確かめ直し：`rows / calls` が減ること。`calls` は変わらない（100ms ごと）。
   - SSE の利用者の確認：`calls` が約 1/10 になること（500ms → 5秒）。LISTEN 経由の確認は、変更があった利用者の数だけ増える。
4. 退会・リージョンの変更の後に SSE が閉じることは、`notification_sse_db_test.go` の `TestNotificationUserChangeListener` で確かめている。実機では、ログイン済みの利用者の SSE をつないだまま `PATCH /api/me` でリージョンを変え、接続がすぐ閉じることを見る。

## 5. 戻す

- アプリのバイナリを前の版に戻す（#21 のロールバック）。
- トリガは残っていても害はない（LISTEN していなければ通知は捨てられる）。消すなら asia2 で次を実行する。

```sql
DROP TRIGGER IF EXISTS users_notify_change ON users;
DROP TRIGGER IF EXISTS users_notify_change_on_delete ON users;
DROP FUNCTION IF EXISTS isureal_notify_user_change();
```

関連：#38、#58、#60、#109
