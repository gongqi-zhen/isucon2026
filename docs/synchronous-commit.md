# synchronous_commit = off の計測と反映（#111、#100）

asia2 の PostgreSQL で、COMMIT のたびに WAL の fsync を待つのをやめる設定です。
repo の `etc/postgresql/conf.d/isucon.conf` には、#100 で `synchronous_commit = off` と `track_wal_io_timing = on` が入っています（コミット 34dd5b5）。
**本番の asia2 に反映するかどうかは、この文書の手順で fsync の待ちを測り、追試の要件と照らしてから決めます。**
本番サーバで実行するコマンドは、手元の Mac から `ssh asia2` で実行します（#21 と同じ前提）。

## 1. 何が変わるか

| | `on`（PostgreSQL の既定） | `off` |
|---|---|---|
| COMMIT が返るとき | WAL をディスクに fsync し終えてから | WAL をメモリ（WAL バッファ）に書いた時点 |
| その間の行ロック | 持ったまま | fsync を待たないので、その分だけ早く離す |
| ほかの接続から結果が見えるとき | COMMIT の時点 | 同じ（fsync を待つかどうかとは関係ない） |
| クラッシュ（電源断、OOM killer で postmaster が落ちる）のとき | COMMIT 済みの結果は残る | 直前 最大 `wal_writer_delay` × 3（既定 200ms × 3 = 約600ms）の間の COMMIT が失われうる |
| 正常な停止（`systemctl stop/restart`、`reboot`） | 残る | 残る（停止の処理で WAL を書き切る） |
| DB の整合性 | 保たれる | 保たれる（`fsync = on`、`full_page_writes = on` のまま。この2つは変えない） |

今の書き込みは、すべて DB の関数1回か autocommit の1文です（#50、#67、#33、#72、#84）。そのため、応答時間のうち COMMIT の fsync が占める割合が、以前より大きくなっています。

- 投稿と RealMoji は、画像（約450KB）を同じトランザクションで INSERT します。その分の WAL を書き終えるまで、本人の users 行とそのリージョンの posting_windows 行（FOR SHARE）を持ち続けます。
- 投稿期間の開始（#72、#85）は、そのリージョンの全利用者分の通知を挿入してから COMMIT します。WAL がいちばん大きいトランザクションです。

## 2. ルール（manual.md）との照らし合わせ

追試の手順（manual.md「追試」）は次のとおりです。

1. 4台のサーバとルーターを**再起動**する
2. 負荷走行を行う
3. もう一度**再起動**する
4. 「負荷走行実行時にアプリケーションに書き込まれたデータが、再起動後に取得できるかどうかを確認します」

- どちらの再起動も正常な停止なので、`off` でもデータは失われません。
- 「競技環境の変更」には「データの永続化、再起動後の動作を維持してください」とあります。異常終了（クラッシュ）まで含めて要求する文言ではないと読めますが、**反映する前に、この文言を運営の最新のレギュレーションでもう一度確かめてください**。異常終了まで含めて要求されていれば、見送ります。
- asia2 は負荷走行中に OOM が起きたことがあります（#16 のコメント）。今は `OOMPolicy=continue` と接続数の削減（合計 104 本）で対策済みです。OOM で PostgreSQL の backend が落ちると、PostgreSQL はクラッシュリカバリをするので、`off` ではその直前 約600ms の COMMIT を失います。反映の前に、走行中の asia2 の `free -m` に余裕があることも確かめてください。

## 3. 計測する（反映の前）

fsync の待ちがどれだけあるかを、`pg_stat_wal` で測ります。`wal_sync_time` を記録するには `track_wal_io_timing = on` が要ります。本番の asia2 にまだ repo の conf を配っていなければ、走行の間だけ `ALTER SYSTEM` で付けます（reload で反映、再起動は不要）。

### 3-1. 走行の前
```bash
ssh asia2 'sudo -u postgres psql -Atc "show synchronous_commit; show track_wal_io_timing;"'
```
期待する出力は `on`、`off`（まだ repo の conf を配っていない場合）です。

```bash
ssh asia2 'sudo -u postgres psql -d isureal' <<'EOF'
ALTER SYSTEM SET track_wal_io_timing = on;
SELECT pg_reload_conf();
SELECT pg_stat_reset_shared('wal');
SELECT pg_stat_statements_reset();
EOF
```
`pg_reload_conf()` の後に `show track_wal_io_timing;` で `on` になっていることを確かめてから、走行を始めます（reload は非同期なので、1秒ほど待つ）。

### 3-2. 負荷走行を1回行う
ポータルから実行し、スコアを控えます。

### 3-3. 走行の後
```bash
ssh asia2 'sudo -u postgres psql -c "SELECT wal_records, wal_bytes, wal_write, wal_sync, round(wal_write_time) AS write_ms, round(wal_sync_time) AS sync_ms, round(wal_sync_time / greatest(wal_sync, 1), 2) AS sync_ms_per_call FROM pg_stat_wal;"'
```

見方：
- `wal_sync` が fsync の回数（ほぼ COMMIT の回数）、`sync_ms` がその合計（ミリ秒）。
- `sync_ms_per_call` が、1回の COMMIT が fsync を待った時間です。EBS（gp3）では 1〜数 ms が目安です。
- 走行が約60秒なので、`sync_ms ÷ 60,000` が「fsync を待っていた接続の平均本数」になります。これが 1 を超えていれば、常にどれかの接続が fsync を待っていた、ということです。

あわせて、ロック待ちの大きさも控えます（変更の前後で比べる）。
```bash
ssh asia2 'sudo -u postgres psql -d isureal' <<'EOF'
SELECT calls, round(mean_exec_time::numeric, 2) AS mean_ms, round(max_exec_time::numeric, 1) AS max_ms,
       left(regexp_replace(query, '\s+', ' ', 'g'), 100) AS query
FROM pg_stat_statements
WHERE query ~* 'for (no key update|share)'
ORDER BY total_exec_time DESC LIMIT 10;
EOF
```

### 3-4. 計測だけで終える場合は戻す
```bash
ssh asia2 'sudo -u postgres psql -c "ALTER SYSTEM RESET track_wal_io_timing; SELECT pg_reload_conf();"'
```

## 4. 判断

次の両方なら反映します。
- `sync_ms_per_call` が 1ms 以上、または `sync_ms ÷ 60,000` が 0.5 以上（COMMIT の待ちが応答時間とロックの時間に見える大きさである）
- 2章の要件の確認が済み、走行中の asia2 のメモリに余裕がある

どちらかが満たせなければ、`isucon.conf` の `synchronous_commit = off` を `on` に戻して配ります（repo に残しておくと、次に conf を配ったときに意図せず効くため）。

### 代案：投稿期間の開始だけ fsync を待たない
`webapp/sql/posting_window_functions.sql` の `isureal_open_posting_window` に、コメントアウトした `PERFORM set_config('synchronous_commit', 'off', true);` を置いています。
DB 全体ではなく、WAL がいちばん大きい投稿期間の開始のトランザクションだけに効かせる案です。失われうるのが投稿期間の開始だけになるので、DB 全体の `off` を見送る場合の代わりになります。有効にするときは、そのコメントを外して `init.sh`（または手動で `psql -f`）で関数を作り直します。

## 5. 反映する（asia2）

repo の `isucon.conf` を配り、reload します（#21 の「1-2」と同じ流儀。`synchronous_commit` と `track_wal_io_timing` は reload で効くので、再起動は不要）。

```bash
scp etc/postgresql/conf.d/isucon.conf asia2:/tmp/isucon.conf
```

```bash
ssh asia2 'sudo install -m 644 -o postgres -g postgres /tmp/isucon.conf /etc/postgresql/16/main/conf.d/isucon.conf && sudo systemctl reload postgresql && sudo -u postgres psql -Atc "show synchronous_commit; show track_wal_io_timing; show fsync; show full_page_writes;"'
```
期待する出力は `off`、`on`、`on`、`on` です。`fsync` と `full_page_writes` が `on` でなければ、すぐに戻してください。

3章で `ALTER SYSTEM SET track_wal_io_timing` を使っていた場合は、conf の値と重複するので消しておきます（`ALTER SYSTEM` の値のほうが優先される）。
```bash
ssh asia2 'sudo -u postgres psql -c "ALTER SYSTEM RESET track_wal_io_timing; SELECT pg_reload_conf();"'
```

### 5-1. 効果を確かめる
負荷走行を1回行い、スコアと、3-3 の `pg_stat_wal`（`wal_sync` が大きく減り、`sync_ms` がほぼ 0 になる）と、ロック待ちの `mean_ms`／`max_ms` を比べます。`POST /api/posts`、`POST /api/posts/:id/realmojis`、`POST /api/admin/posting-window` の応答時間も比べます。

### 5-2. 再起動してデータが残ることを確かめる
走行の直後（次の初期化の前）に、直前の投稿を控えてから PostgreSQL を止めて起動し、残っていることを確かめます。

```bash
ssh asia2 '. ~/env.sh; export PGPASSWORD=$ISUREAL_DB_PASSWORD; psql -h 127.0.0.1 -U isureal -d isureal -Atc "SELECT id, posted_at FROM posts ORDER BY created_at DESC LIMIT 3;"'
```

```bash
ssh asia2 'sudo systemctl restart postgresql@16-main && systemctl is-active postgresql@16-main'
```

```bash
ssh asia2 '. ~/env.sh; export PGPASSWORD=$ISUREAL_DB_PASSWORD; psql -h 127.0.0.1 -U isureal -d isureal -Atc "SELECT id, posted_at FROM posts ORDER BY created_at DESC LIMIT 3;"'
```
同じ3件が出れば、正常な停止では失われないことが確かめられます。追試と同じ条件にするなら、`sudo reboot` で asia2 を再起動してから同じ確認をします（起動後に `envcheck` も通すこと）。

### 5-3. 計測を終えたら
`track_wal_io_timing` は WAL の書き込みごとに時刻を取るので、計測を終えたら `isucon.conf` で `off` に戻して配ってもよいです。

## 6. 戻す

`isucon.conf` の `synchronous_commit = off` を `on` に変えて 5章と同じ手順で配ります。すぐ戻すだけなら、次でも戻せます（conf を配り直すまでの一時的な措置）。
```bash
ssh asia2 'sudo -u postgres psql -c "ALTER SYSTEM SET synchronous_commit = on; SELECT pg_reload_conf();" -Atc "show synchronous_commit;"'
```
`ALTER SYSTEM` の値は conf より優先されるので、戻し終えたら `ALTER SYSTEM RESET synchronous_commit` で消しておきます。

関連：#100、#111、#21、#5、#16
