# asia1 と asia2 の役割分担（#15）

asia の API を、asia1 と asia2 の Go アプリに振り分ける構成案です。
asia1 の nginx が受けたリクエストを、ルーター経由で asia2 の Go アプリ（`ISUREAL_REGION=asia`）にも転送します。

## 1. ルール上の判断（`manual.md`）

**可と判断する。**

| 確認した点 | `manual.md` の記述 | 判断 |
|---|---|---|
| リージョン | 「asia1とasia2は同一リージョンとして扱います」「サーバのリージョンは変更できません」 | asia2 で動かすのは asia のアプリだけ。どのサーバのリージョンも変えない |
| 公開先 | ルーターは asia 宛てを「asia1」へ転送する。「固定リージョン、公開先、ルーター経由の通信を維持してください」 | 公開先は asia1 の nginx のまま。ルーターの設定には触れない |
| ルーター経由 | 「ユーザーからアプリケーションへの通信と、サーバ間の通信はルーターを経由します」。禁止事項に「ルーターを迂回するサーバ間通信」 | asia1 から 192.168.4.10 宛ての通信は、DB の通信と同じくゲートウェイ（192.168.1.1）を経由する（`ip route get 192.168.4.10` で確認した）。asia2 のアプリから DB へは同じサーバ内の通信で、サーバ間の通信ではない |
| 遅延の回避 | 禁止事項に「サーバのリージョン、通信遅延、帯域制限を変更または回避すること」 | リージョンをまたぐ通信は1つも増減しない。asia2 の `env.sh` は運営が用意した初期状態で `ISUREAL_DB_HOST=127.0.0.1` |
| 全台の使用 | 「初期状態で全てのサーバを使用します」「ベンチマーク対象として、任意の1台だけを選ぶ構成ではありません」 | 4台とも使い続ける |
| 初期化 | 「独自に追加したキャッシュや複製先も初期化してください」「全リージョンで整合した初期状態へ戻してください」 | 3章で確認した |

europe と america のアプリを asia2 で動かす案（#14 の案A）とは違い、どのリクエストも、ルーターが決めたリージョンのサーバで処理されます。
それでも心配なら、ポータルの質問で確認してから反映してください。

## 2. 計測（2026-10-31、負荷走行2回、読み取りのみ）

全台が x86_64、2コア、メモリ 3.7GiB。asia1 から asia2 への RTT は 1.09ms（ゲートウェイまでは 0.05ms）。

負荷テスト中（約60秒）の値は次のとおり（2回目、Score 12337、`vmstat 1` と `pidstat 5`）。

| | CPU 使用（us+sy） | アイドル（p10 / p50） | iowait | 主なプロセス | 空きメモリ（available） |
|---|---|---|---|---|---|
| asia1 | 平均 84% | 5% / 17% | 0% | isureal が平均 127%（2コアで200%）、nginx が 7% | 約 3.0GiB |
| asia2 | 平均 20% | 17% / 42% | **平均 41%** | postgres。ディスクから毎秒 20〜40MB を読む | 約 1.3GiB |
| europe1、america1 | ほぼ 0% | — | — | 起動後の isureal の CPU 時間は合計 7秒 | 約 3.1GiB |

1回目（Score 14033）も、asia1 は負荷の間アイドルが 0〜20% だった。

- **asia1 は CPU が限界に近い。** 律速になっているのは asia1 の Go アプリ。
- **asia2 は CPU に1コア以上の余裕がある。一方で、ディスクの読み取りを待っている。** DB は 17GB で、そのうち 14GB が `image_objects`。shared_buffers の 1GB とページキャッシュには収まらない。
- europe と america にはほとんど負荷がかかっていない。

## 3. 構成

```
ルーター → asia1 nginx ─┬─ /api/initialize, /api/images/ → asia1 の Go（127.0.0.1:8080）
                        └─ その他の /api/（SSE を含む） → asia1 の Go : asia2 の Go（192.168.4.10:8080）= 1 : 1
asia2 の Go → PostgreSQL（127.0.0.1:5432）
```

- `etc/nginx/sites-available/isureal-split.conf`：asia1 用。`isureal.conf` の代わりに有効にする。
- `etc/systemd/asia2/isureal-go.service.d/10-asia2.conf`：asia2 用の drop-in。
  - 待ち受けは `192.168.4.10:8080` だけ。
  - DB の接続数は 48（64 × 3 + 48 = 240、上限は `max_connections = 256`）。
  - 画像キャッシュは 64MB、`GOMEMLIMIT=512MiB`。
- アプリの変更は、環境変数を2つ足しただけ。`ISUREAL_LISTEN_ADDR`（既定 `:8080`）と `ISUREAL_DB_MAX_CONNS`（既定 64）。既定値の動作は今と同じ。

### 画像を asia1 で返す理由

- 画像本体のキャッシュは、プロセスごとに持つ。振り分けると、キャッシュに当たる率が下がる。
- asia2 は空きメモリが少なく、ディスクの読み取りを待っている。キャッシュを持たせると、PostgreSQL のページキャッシュがさらに減る。
- 画像は署名の検証（HMAC 1000回）がキャッシュに当たれば軽い。CPU を多く使うのは、署名付き URL を作るフィードなどの API のほう。

### 複数のプロセスでの整合性

europe1 と america1 は初期化のリクエストを受けないので、もともと「初期化でキャッシュが消えないプロセス」が2つあります。asia2 のプロセスも、それと同じ条件になります。

| 状態 | 複数のプロセスで正しく動く理由 |
|---|---|
| `imageObjectCache`（画像本体） | ID ごとに中身が変わらない。ID は UUID で、走行をまたいでも使い回されない |
| `imageOwnerKeyCache`、`signedURLCache`、`verifiedSignatureCache` | 画像の所有者も、`secret_key_hash` も変わらない。そのため、同じ ID と同じ鍵からは同じ結果になる |
| `imageValidityCache`（削除・差し替え・退会の反映） | 有効期間は2秒。プロセスごとに DB で確かめ直すので、5秒の猶予に収まる |
| SSE の通知ハブ | 投稿期間の変化を DB から 100ms ごとにポーリングする。別リージョンから始まった投稿期間を検知するのと同じ仕組みで、asia1 で始まった投稿期間にも asia2 のハブが気づく（逆も同じ） |
| IP の BAN | DB の `ip_bans` を 200ms ごとに読み直す |
| セッション | DB に保存する。プロセス内には持たない |

- **初期化**：nginx で `/api/initialize` を asia1 に固定する。init.sh は1回だけ実行され、DB を初期状態に戻す。
  - asia1 のキャッシュは、初期化の処理の中で消える。
  - asia2 のキャッシュは、上の表のとおり、消えなくても DB と食い違わない。
  - 例外：初期化の直後2秒間に限り、asia2 は前回の走行で作られた画像を返しうる（europe1 と america1 も同じ）。ただし、画像は asia1 で返す構成なので、asia2 が画像を返すことはない。
- **SSE**：長い接続は、重みつきのラウンドロビンで両方に分かれる。待機中の接続は CPU をほとんど使わない。
  - `proxy_buffering off` で、通知をすぐに転送する。
  - `proxy_read_timeout` は今と同じ既定値の60秒。
- **asia2 が落ちたとき**：接続できなければ（拒否、または1秒でタイムアウト）、nginx が asia1 に回す。
  - リクエストを送ったあとの失敗では、POST を再送しない（二重に投稿しない）。
  - 追試で4台を再起動したとき、asia2 のアプリの起動が遅れても、asia1 だけで応答できる。

## 4. 反映手順

手元の Mac で、repo のルートから実行します（#21 と同じ前提）。**反映の前に、ポータルで負荷走行を1回行い、スコアを記録してください。**

### 4-1. 全台のアプリを、この PR の版にする
環境変数の既定値は今と同じなので、asia1、europe1、america1 は今と同じ動作をします。手順は #21 の「共通：アプリのデプロイ」と同じです。

```bash
(cd webapp/go && go test ./... && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../../isureal.linux ./cmd/isureal)
```

### 4-2. asia2 にアプリを配置し、起動する
```bash
ssh asia2 'cp ~/webapp/go/isureal ~/webapp/go/isureal.prev'
```

```bash
rsync -az isureal.linux asia2:/home/isucon/webapp/go/isureal && rsync -az --delete webapp/sql/ asia2:/home/isucon/webapp/sql/
```

```bash
scp etc/systemd/isureal-go.service asia2:/tmp/isureal-go.service && scp etc/systemd/asia2/isureal-go.service.d/10-asia2.conf asia2:/tmp/10-asia2.conf
```

```bash
ssh asia2 'sudo install -m 644 /tmp/isureal-go.service /etc/systemd/system/isureal-go.service && sudo install -D -m 644 /tmp/10-asia2.conf /etc/systemd/system/isureal-go.service.d/10-asia2.conf && sudo systemctl daemon-reload && sudo systemctl enable --now isureal-go && sleep 2 && systemctl is-active isureal-go && ss -ltnp | grep 8080'
```
`192.168.4.10:8080` で待ち受けていることを確かめます（`*:8080` ではないこと）。

### 4-3. asia1 から asia2 のアプリに届くことを確認する
```bash
ssh asia1 'curl -s -o /dev/null -w "%{http_code}\n" http://192.168.4.10:8080/api/me'
```
`401` などのアプリの応答が返れば届いています。接続できない場合は、セキュリティグループで asia1 から asia2 への TCP/8080 を許可します（変更してはいけないのは SSH、HTTPS、ルーターの TCP/8080 だけ）。

### 4-4. asia1 の nginx を切り替える
```bash
scp etc/nginx/sites-available/isureal-split.conf asia1:/tmp/isureal-split.conf
```

```bash
ssh asia1 'sudo install -m 644 /tmp/isureal-split.conf /etc/nginx/sites-available/isureal-split.conf && sudo ln -sfn /etc/nginx/sites-available/isureal-split.conf /etc/nginx/sites-enabled/isureal.conf && sudo nginx -t && sudo systemctl reload nginx'
```
`sites-enabled/isureal.conf` というリンクの名前はそのままにして、リンク先だけを変えます。

### 4-5. 確認する
- ブラウザで、ログイン、投稿、フィード、画像の表示を確認する。
- 負荷走行を行い、スコアを比べる。走行中は、asia1 と asia2 の両方で `vmstat 1` を見る。
- asia2 の iowait が増えてスコアが下がる場合は、`isureal-split.conf` の asia2 の `weight` を下げる（例：asia1 を 2、asia2 を 1）。

## 5. 元に戻す手順

順番に実行します。4-4 だけ戻せば、asia2 のアプリには何も届かなくなります。

```bash
ssh asia1 'sudo ln -sfn /etc/nginx/sites-available/isureal.conf /etc/nginx/sites-enabled/isureal.conf && sudo nginx -t && sudo systemctl reload nginx'
```

```bash
ssh asia2 'sudo systemctl disable --now isureal-go && sudo rm -f /etc/systemd/system/isureal-go.service.d/10-asia2.conf && sudo systemctl daemon-reload && systemctl is-active isureal-go; systemctl is-enabled isureal-go'
```
`inactive` と `disabled` が出れば、初期状態と同じです。

アプリのバイナリを戻す場合は、`isureal.prev` を `isureal` に戻して再起動します（#21 のロールバックと同じ）。

## 6. 未検証のこと

- この構成での負荷走行（スコアが上がるかどうか）。asia2 は CPU に余裕がある一方、ディスクの読み取りを待っているので、アプリを足すと PostgreSQL が遅くなるおそれがある。
- asia1 から asia2 の TCP/8080 に接続できるかどうか。待ち受けがない状態で接続が拒否される（タイムアウトしない）ことは確認した。セキュリティグループで届いていると考えられるが、実際にアプリへ届くかは試していない。
- 1:1 という重みが最適かどうか。
- asia2 のアプリのメモリ使用量（`GOMEMLIMIT=512MiB` で足りるか）。
- nginx の設定は、asia1 で `nginx -t` の構文チェックだけを通した（80番ポートの bind は権限がないので失敗する）。
