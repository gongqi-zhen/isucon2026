# asia でもセッションキャッシュを有効にする（Issue #109）

セッションキャッシュ（`webapp/go/session_cache.go`、#38・#58・#62）は、既定では europe と america だけで有効です。
asia2 のアプリを止めている今は asia も1プロセスなので、asia でも有効にできます。このページは、その確認と反映と戻し方の手順です。

## 1. 何が変わるか

| 項目 | 今（asia はキャッシュしない） | asia でも有効にした後 |
|---|---|---|
| asia1 の認証付きリクエスト | 毎回、セッションの確認で DB を1往復（約1ms。DB の接続を1本使い、asia2 の PostgreSQL の CPU も使う） | TTL（400ms）以内に確かめたトークンは DB を引かない |
| asia1 の `GET /api/me` | さらに `getUser` で1往復 | キャッシュした利用者の行で応答する（#58 と同じ） |
| asia の利用者のログイン・ログアウト・退会・`PATCH /api/me`・プロフィール画像・認定 | 待たない（asia はキャッシュしないので） | `ISUREAL_SESSION_CACHE_SOLE=1` なら待たない。付けないと、コミットしてから 400ms 待って応答する |
| 確かめ直しのクエリ（100ms ごとに、持っている全トークンを1クエリ） | europe1 と america1 だけ | asia1 でも走る。asia はいちばん利用者が多いので、このクエリがいちばん大きくなる（#110 で測る） |
| 初期化（`/api/initialize`） | TTL 待つ（europe・america のため） | 同じ |

正しさは、europe と america で使っているのと同じ約束（`session_cache.go` の冒頭の 1〜3）で保たれます。
asia で有効にした状態のテストは `webapp/go/session_cache_asia_test.go`（DB なし）と `session_cache_asia_db_test.go`（PostgreSQL と HTTP ハンドラ）にあります。

## 2. 反映の前に確かめること（読み取りだけ）

### 2-1. europe1 と america1 に `ISUREAL_SESSION_CACHE_SOLE=1` があるか

今はどのリージョンも1プロセスなので、europe1 と america1 には付けてよい設定です。付いていなければ、
ログイン・ログアウト・退会・`PATCH /api/me`・プロフィール画像の設定と削除のたびに 400ms 待っています（#62）。

```bash
for h in europe1 america1 asia1; do echo "== $h"; ssh $h 'grep -H SESSION_CACHE ~/env.sh /etc/systemd/system/isureal-go.service.d/*.conf 2>/dev/null; systemctl show isureal-go -p Environment | tr " " "\n" | grep SESSION_CACHE'; done
```

- `ISUREAL_SESSION_CACHE_SOLE=1` が europe1 と america1 に出なければ、3章の手順で一緒に足します。
- `ISUREAL_SESSION_CACHE_REGIONS` がどこかに書かれていたら、その値を控えます（3台とも同じ値でなければなりません）。
- europe1 と america1 には手書きの drop-in `/etc/systemd/system/isureal-go.service.d/20-release.conf`（`GOMEMLIMIT` など）があります。上書きしないこと。

### 2-2. asia2 のアプリが止まっていること

```bash
ssh asia2 'systemctl is-active isureal-go; systemctl is-enabled isureal-go'
```

`inactive` と `disabled` であること。動いていれば asia のプロセスが2つあるので、`ISUREAL_SESSION_CACHE_SOLE` は付けられません（5章）。

### 2-3. 確かめ直しのクエリの負荷（#110）

europe1 の確かめ直しのクエリが asia2 の PostgreSQL をどれだけ使っているかを、`pg_stat_statements` で見ます（手順は #110）。
asia の有効なトークンの数は次で数えられます。

```sql
SELECT u.region, count(*) FROM login_sessions s JOIN users u ON u.id = s.user_id WHERE s.active GROUP BY u.region;
```

asia の分が europe の数倍あれば、確かめ直しのクエリもその倍率で大きくなります。#110 の対策（最近使ったトークンだけを確かめ直す）を先に入れるかどうかを、ここで決めます。

## 3. 反映する

`ISUREAL_SESSION_CACHE_REGIONS` は**全サーバで同じ値**にします。書く側（ログインなど）は、この設定で「待つかどうか」を決めるので、
1台だけ違うと、ほかのサーバのキャッシュに古いセッションが残りえます。そのため、3台の `env.sh` を変えてから、3台をまとめて再起動します。

反映の前に、ポータルで負荷走行を1回行い、スコアを記録してください。

```bash
for h in asia1 europe1 america1; do
  echo "== $h"
  ssh $h 'cp ~/env.sh ~/env.sh.prev && sed -i -e "/^ISUREAL_SESSION_CACHE_REGIONS=/d" -e "/^ISUREAL_SESSION_CACHE_SOLE=/d" ~/env.sh && printf "ISUREAL_SESSION_CACHE_REGIONS=asia,europe,america\nISUREAL_SESSION_CACHE_SOLE=1\n" >> ~/env.sh && tail -3 ~/env.sh'
done
```

```bash
for h in asia1 europe1 america1; do ssh $h 'sudo systemctl restart isureal-go && sleep 2 && systemctl is-active isureal-go && systemctl show isureal-go -p Environment | tr " " "\n" | grep SESSION_CACHE'; done
```

3台とも `ISUREAL_SESSION_CACHE_REGIONS=asia,europe,america` と `ISUREAL_SESSION_CACHE_SOLE=1` が出ることを確かめます。
（`systemctl show` に出るのは unit と drop-in の `Environment=` だけなので、env.sh の値が出ないときは `ssh asia1 'cat /proc/$(pgrep -x isureal)/environ | tr "\0" "\n" | grep SESSION_CACHE'` で見ます。）

### 反映後の確認

- ブラウザか curl で、asia の利用者のログイン → `GET /api/me` → ログアウト → 同じトークンで `GET /api/me` が 401 になること
- 負荷走行を1回行い、スコアと次を比べる
  - `pg_stat_statements` で、セッションの確認のクエリ（`SELECT s.user_id, s.token_hash, s.created_at, u.region, ... FROM login_sessions s JOIN users u`）の `calls` が、asia の分だけ減ること
  - 確かめ直しのクエリ（`unnest($1::text[], $2::text[])` を含むもの）の `total_exec_time` と `rows`
  - asia2 の PostgreSQL の CPU（`pidstat -u 5`）と、asia1 の `isureal` の CPU

## 4. 元に戻す

```bash
for h in asia1 europe1 america1; do ssh $h 'cp ~/env.sh.prev ~/env.sh && sudo systemctl restart isureal-go && sleep 2 && systemctl is-active isureal-go'; done
```

3台まとめて戻します（`ISUREAL_SESSION_CACHE_REGIONS` は全サーバで同じ値にするため）。

## 5. asia2 のアプリを再び動かすとき

`docs/asia-role-split.md` の構成（asia1 の nginx から asia2 のアプリにも振り分ける）に戻すと、asia のプロセスが2つになります。
その前に、**3台すべての `env.sh` から `ISUREAL_SESSION_CACHE_SOLE` を外して再起動**してください。

```bash
for h in asia1 europe1 america1; do ssh $h 'sed -i -e "/^ISUREAL_SESSION_CACHE_SOLE=/d" ~/env.sh && sudo systemctl restart isureal-go && sleep 2 && systemctl is-active isureal-go'; done
```

- 外し忘れると、asia1 でログアウトしたトークンが、asia2 のプロセスのキャッシュで最大 TTL（400ms）の間 有効のままになります（europe と america も、asia2 のプロセスが asia の利用者をキャッシュしうるので同じ）。
- `ISUREAL_SESSION_CACHE_REGIONS=asia,europe,america` は、そのままでも正しく動きます（書く側が TTL 待つようになるだけ）。asia2 のアプリは env.sh の値をそのまま使うので、asia2 の `env.sh` にも同じ値を書きます。
- asia2 の drop-in `etc/systemd/asia2/isureal-go.service.d/10-asia2.conf` のコメントにも、この注意を書いてあります。

## 6. テスト

asia で有効にした状態の DB テストは、設定がプロセスの起動時に決まるので、別のプロセスで実行します。

```bash
cd webapp/go && ISUREAL_TEST_DSN='postgres://isureal:isureal@127.0.0.1:55438/isureal?sslmode=disable' \
  ISUREAL_SESSION_CACHE_REGIONS=asia,europe,america ISUREAL_SESSION_CACHE_SOLE=1 \
  go test -count=1 -run TestSessionCacheAsiaDB -v ./...
```

確かめていること：asia の利用者のログアウト・再ログイン・退会・`PATCH /api/me`（表示名とリージョン）が待たずに応答し、その直後から古いトークンや古い行が使われないこと。
europe の利用者を asia のプロセスで無効にしたときは今まで通り待つこと。`login_sessions` を空にして `resetSessionCache` を呼んだ後は古いトークンが使えないこと。
ログアウトの間に並行して読み続けても、応答の後に1回も通らないこと。
