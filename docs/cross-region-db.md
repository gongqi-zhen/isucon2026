# europe と america から DB への往復を減らす（Issue #14）

調べたのは 2026-10-31 12:20〜13:00 JST。本番サーバでは読み取りのコマンドしか実行していない。

## 1. 計測値

### RTT（europe1、america1 → asia2:5432）

| 送信元 | ping | `psql` 内の `SELECT 1` | 接続して `SELECT 1` を1回実行 |
|---|---|---|---|
| asia1 | 1.0ms | 1.0ms | 51ms |
| europe1 | 100.8ms | 100.8〜101.1ms | 837ms |
| america1 | 100.9ms | 100.8〜101.1ms | 838ms |

- europe1 と america1 の間も 100.6ms あった。asia2 を経由しない場合でも、リージョンをまたぐと約100ms かかる。
- 帯域は、画像1枚（約410KB、hex で 820KB）を同じ接続で連続して取得して測った。1回目は 517ms、2回目は 136ms（RTT を引くと約36ms）。帯域より RTT と TCP のスロースタートの影響のほうが大きい。
  - 各サーバとも `net.ipv4.tcp_slow_start_after_idle = 1`（既定値）。
- 新しく投稿された画像の平均は約450KB（`pg_column_size`、直近200件）。

### `pg_stat_statements`（03:23 UTC にリセットした後、負荷走行1回分を含む）

- FK チェック（`FOR KEY SHARE OF x`）を除くと、文の数は 210,625 だった。セッションの取得（＝認証付きリクエスト）は 19,852 回、画像の所有者の取得は 12,553 回、ログインは 6,119 回。
  - このため、1リクエストあたりの往復は平均で約5.5回になる（BEGIN と COMMIT を含む）。
  - この値は全リージョンの合計で、リージョン別には分けられない。
- 実行時間はほとんどが 1ms 未満だった。遅いのは、どれも行ロックの待ちである。

| クエリ | calls | mean |
|---|---|---|
| `SELECT ... FROM posting_windows WHERE region=$1 AND ended_at IS NULL FOR UPDATE`（投稿） | 770 | **697.6ms** |
| `SELECT id FROM users WHERE id IN ($1,$2) ... FOR NO KEY UPDATE`（関係の操作） | 1,305 | **178.4ms** |
| `SELECT id FROM users WHERE id=$1 AND status=$2 FOR NO KEY UPDATE` | 5,405 | 33.2ms |
| `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE` | 1,411 | 12.5ms |

ロックを持つ時間は「ロックの後の文の数 × 100ms」になる。このため、europe と america のトランザクションではロックを長く持ち、その待ちがほかのリージョンにも広がっている。

### 1リクエストあたりの往復回数（コードから数えた。S はセッションの取得で1往復）

| ルート | 往復 | ロックを持つ間の往復 |
|---|---|---|
| GET /api/feeds/:feed | S + 1〜3 | 0 |
| GET /api/images/:id | キャッシュに当たれば 0、2秒ごとに 1 | 0 |
| POST /api/posts | S + 8 | user 6、**そのリージョンの posting_windows の行 5** |
| POST /api/posts/:id/comments | S + 6 | 3（投稿者の users の行） |
| POST /api/posts/:id/realmojis | S + 9 | 6（投稿者の users の行） |
| POST /api/auth/login | 7 | 5 |
| PUT /api/follows/:uid | S + 6 | 5（認定公開アカウントの行） |
| POST /api/friend-requests | S + 6〜7 | 5〜6 |
| GET /api/memories | S + 3 | 0 |
| GET /api/users/:id | S + 3 | 0 |

### 見積もり

europe と america のリクエストの応答時間は、ほぼ「往復回数 × 100ms ＋ ロックの待ち」になる。

- フィードは 200〜400ms、投稿は 900ms ＋ ロックの待ち（平均 700ms）。
- DB での実行時間は 1ms 未満なので、**応答時間の95%以上がリージョン間の遅延と、それが原因のロックの待ち**になる。
  - 計測していないもの：負荷をかけた状態の pprof。負荷走行中の CPU の内訳が必要なら、走行のタイミングで `go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30` を europe1 で実行する。
- 投稿は、リージョンごとに1行しかない、開いている `posting_windows` の行を `FOR UPDATE` で約500ms 持つ。そのため、**1つのリージョンでは毎秒2件ほどしか投稿できない**。

## 2. ルール上の判断（`manual.md`）

| 案 | 判断 | 根拠 |
|---|---|---|
| A：asia2 で europe と america のアプリを動かし、europe1 と america1 の nginx から転送する | **グレー。運営の回答が出るまで実施しない** | 禁止事項に「サーバのリージョン、通信遅延、帯域制限を変更または回避すること」とある。競技環境の変更には「固定リージョン、公開先、ルーター経由の通信を維持してください」とある。通信はルーターを経由し、遅延も1回は払う。それでも、europe の処理を asia で行うことは、リージョンの「回避」とみなされるおそれがある。 |
| B：europe1 と america1 に読み取りレプリカを置く | **ルール上は可（技術的なリスクは大きい）** | 「サーバ内のソフトウェア、DBスキーマ、インデックス、保存方式…を変更できます」「キャッシュの追加も可能です」「独自に追加したキャッシュや複製先も初期化してください」。ただし「書き込みが成功した後の読み取りには、その結果を反映します。リージョンをまたぐ場合も同じです」とあるので、`synchronous_commit = remote_apply` で2台への同期レプリケーションが必要になる。 |
| C：アプリで往復の回数を減らし、ロックを持つ時間を短くする | **可** | 「アプリケーションの実装を変更できます」。外部仕様（ステータス、JSON、閲覧制限）は変えない。 |
| （参考）europe と america の `tcp_slow_start_after_idle=0` | **グレー。質問してから行う** | ルーターの遅延や帯域は変えない。それでも「帯域制限を…回避」と受け取られる可能性があるので、念のため確認する。 |

### 案Bを今は勧めない理由

- 全部の書き込みの COMMIT で、遠いほうのレプリカの適用を待つ。
  - asia の書き込みは、1ms から約100ms 以上に遅くなる。
  - europe からの書き込みは、COMMIT がさらに約100ms 遅くなる。
  - ロックを持つ時間も延びる。
- 追試では、4台とルーターを再起動する。レプリカが1台でも上がらないと、全部の書き込みが止まり、FAIL するおそれがある。
- 2コアのアプリサーバで PostgreSQL も動かすことになる。また、読み取り専用の経路をアプリで分ける必要がある。
- asia1 と asia2 の役割の分担（#15）とも衝突する。

## 3. 推奨案（案C）と優先順位

1. **投稿の `posting_windows ... FOR UPDATE` を `FOR SHARE` に変える**（`post_api.go`。#13 のセッションの担当なので、この PR では変えない）
   - 投稿期間を閉じる `UPDATE posting_windows SET ended_at` は、`FOR SHARE` でも待たされる。
   - 1人1件の制約は、`lockUser` で守られている。
   - したがって、意味を変えずに、同じリージョンの投稿を並列に実行できる。いちばん効果が大きい。
   - あわせて、`getPost` を `INSERT ... RETURNING` に置き換え、ErrNoRows のときに同じクエリを再試行する処理を消すと、ロックを持つ間の往復も減る。
2. **ログインの往復を 7 から 5 にする（この PR）**
   - ログインでは、同じユーザーを2回目にロックする処理と、`getLoginSession` を消す。
   - セッションの無効化と追加は、CTE の1文にまとめる。
   - その結果、ロックを持つ間の往復は 5 から 3 になる。
3. **フォローの往復を S+6 から S+4 にする（この PR）**
   - 相手の種別の確認、ブロックの確認、既存のフォローの確認を1文にまとめる。
   - その結果、人気のある認定公開アカウントの行のロックを持つ間の往復は 5 から 3 になる。
4. 次に効果がありそうなもの（別の PR にする）
   - コメントと RealMoji：投稿者の行のロックを持つ間に `loadPostAccess` を実行しないようにする。RealMoji では `getUser` を重ねて実行しないようにする。
   - フレンド申請：`getFriendRequest` を `RETURNING` に置き換える。
   - 登録：`LOCK TABLE users` を持つ間の往復を減らす。
5. やらないこと
   - セッションをプロセス内にキャッシュする。ログアウト、再ログイン、凍結がリージョンをまたいですぐ反映されなくなり、仕様に反するおそれがある。

## 4. 運営への質問の文案

> **質問1（構成）**
> europe1 と america1 の nginx が受けたリクエストを、ルーターを経由して（192.168.4.10 宛て）asia2 で動かす `ISUREAL_REGION=europe` と `america` のアプリケーションに転送する構成は認められますか。
> ユーザーからの通信は、今と同じくルーターから europe1 と america1 に届き、サーバ間の通信もすべてルーターを経由します。
> 「固定リージョン…を維持してください」「サーバのリージョン…を回避すること」に当たるかどうかを確認させてください。

> **質問2（レプリカ）**
> europe1 と america1 に PostgreSQL のレプリカを置き、asia2 からルーター経由で同期レプリケーションを行い、読み取りを各リージョンのレプリカで行う構成は、レギュレーション上問題ありませんか。

> **質問3（カーネル設定）**
> 各サーバで `net.ipv4.tcp_slow_start_after_idle` などの TCP のパラメータを変えることは、「通信遅延、帯域制限を…回避すること」に当たりますか。

## 5. この PR の反映と、元に戻す手順

変更したのは、`webapp/go/session.go` と `webapp/go/relationship_api.go` のアプリのコードだけ。設定ファイル、systemd の unit、DB のスキーマは変えていない。

反映（Issue #21 の「共通：アプリのデプロイ」と同じ。手元の repo のルートから、asia1、europe1、america1 の3台に反映する）：

```sh
(cd webapp/go && go test ./... && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../../isureal.linux ./cmd/isureal)
for h in asia1 europe1 america1; do
  echo "== $h"
  ssh $h 'cp ~/webapp/go/isureal ~/webapp/go/isureal.prev'
  rsync -az isureal.linux $h:/home/isucon/webapp/go/isureal
  ssh $h 'sudo systemctl restart isureal-go && sleep 2 && systemctl is-active isureal-go'
done
```

反映した後は負荷走行を1回実行し、スコアと `pg_stat_statements` の `login_sessions` と `follows` の行を、反映の前と比べる。

元に戻す：

```sh
for h in asia1 europe1 america1; do
  ssh $h 'cp ~/webapp/go/isureal.prev ~/webapp/go/isureal && sudo systemctl restart isureal-go && systemctl is-active isureal-go'
done
```

確認したこと：

- `go build`、`go vet`、`go test` が通った。
- 新しい SQL 2本を、ローカルの postgres:16 にスキーマと migrate を流した DB で実行した。旧セッションが `replaced` で無効になり、新しいセッションだけが active になった。フォローの確認クエリは1行を返した。

確認していないこと：

- 負荷走行での効果。
