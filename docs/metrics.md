# 接続プールとキャッシュの指標（#108）

アプリは、pprof と同じポート（`ISUREAL_PPROF_ADDR`、既定 `127.0.0.1:6060`）の `/debug/vars` で、
接続プールとプロセス内のキャッシュの状態を JSON で返します（`webapp/go/metrics.go`）。
値は読まれたときに計算します。リクエストの処理に足しているのは、各キャッシュのアトミックなカウンタの加算だけです。

## 取り方

各サーバで、負荷走行の前と走行中（数回）と後に取ります。127.0.0.1 だけで待ち受けているので、サーバの中で実行します。

```bash
curl -s 127.0.0.1:6060/debug/vars | jq '{isureal_db, isureal_caches, isureal_session_cache, isureal_notification_hub}'
```

走行の間、5秒ごとに記録する例：

```bash
for i in $(seq 1 24); do date +%T; curl -s 127.0.0.1:6060/debug/vars | jq -c '{db: .isureal_db, img: .isureal_caches.image_object, sess: .isureal_session_cache, sse: .isureal_notification_hub}'; sleep 5; done | tee /tmp/metrics-$(hostname).log
```

カウンタは起動からの累計です。走行の前後の差を見ます。初期化（`/api/initialize`）でキャッシュの中身は消えますが、カウンタは消えません。

## 項目

| キー | 中身 | 見るところ |
|---|---|---|
| `isureal_db` | `database/sql` の `DB.Stats()` | `WaitCount`（接続が空くのを待った回数）、`WaitDuration`（その合計、ナノ秒）、`InUse`、`Idle`、`MaxOpenConnections` |
| `isureal_caches.<name>` | LRU キャッシュごとの `len`、`cost`、`max_cost`、`hits`、`misses`、`evictions` | `signed_url`、`verified_signature`、`image_owner_key`、`image_object`（cost はバイト数）、`image_validity`、`jpeg_verdict` |
| `isureal_session_cache` | `enabled`、`region`、`entries`、`max`、`hits`、`misses` | europe と america だけ `enabled` が true（`ISUREAL_SESSION_CACHE_REGIONS`） |
| `isureal_notification_hub` | `waiters`（SSE で待機中の接続の数） | nginx の `worker_connections`（#106）と比べる |

`evictions` は、上限を超えて追い出した件数です。同じキーの置き換えや、期限切れ（`misses` に数える）、`Remove`、初期化の `Reset` は含みません。

## 結果からの判断

### DB の接続数（`ISUREAL_DB_MAX_CONNS`）
- 走行の前後で `isureal_db.WaitCount` が増えていれば、接続が足りずに待っています。`WaitDuration` の増分 ÷ `WaitCount` の増分が、1回あたりの待ちです。
- 今の設定は asia1 = 24、europe1 = america1 = 40（各サーバの `~/env.sh`。合計 104）。asia1 はリクエストが europe1 の 12〜15 倍（#35）です。
- asia1 で待ちが出ていれば、合計を増やさずに配分を変えます（例：asia1 = 40、europe1 = america1 = 32）。PostgreSQL の接続1本は約 12MB で、asia2 は OOM を起こしたことがあります（#16 のコメント）。全台の合計が `max_connections = 256` と asia2 のメモリに収まることを、asia2 の `free -m` で確かめます。
- europe1 と america1 で `WaitCount` が 0 のままなら、その分を asia1 に回せます。

### 画像本体のキャッシュ（`ISUREAL_IMAGE_CACHE_BYTES`、既定 512MB）
- `image_object.evictions` が走行の間に増え続けていれば、上限に当たっています。`misses` のうち追い出しが原因の分は、DB からリージョンをまたいで読み直しています（1枚 約450KB）。
- asia1 は空きメモリ 約3GiB、`GOMEMLIMIT=2GiB` なので、1GiB まで増やせます。europe1 と america1 も同程度の空きがあります（`GOMEMLIMIT` は drop-in で 2600MiB）。
- `evictions` が 0 で `cost` が `max_cost` に届いていなければ、増やしても効果はありません。

### 署名のキャッシュ（`signed_url`、`verified_signature`）
- `misses` が多ければ、その回数だけ HMAC を 1,000 回計算しています（#71）。`evictions` が増えていれば上限（200,000 件）を増やします。
- `verified_signature` の `misses` は、署名付き URL を作ったプロセスと、画像を取りにきたプロセスが違うときにも増えます。

### セッションキャッシュ
- `entries` は、確かめ直しのクエリ（100ms ごと）が1回に送るトークンの数です。#110 の計測の材料になります。
- `misses` が多ければ、TTL（400ms）を過ぎたエントリを引き直しています。

### SSE の接続数
- `waiters` の最大値が、nginx の `worker_connections` の半分（アプリへの接続も1つに数える）に近ければ、nginx の上限を上げます（#106）。
