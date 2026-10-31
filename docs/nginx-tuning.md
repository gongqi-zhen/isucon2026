# nginx の設定（#99、#106）

アプリの手前の nginx で、asia1 の CPU と画像の転送に効く3つの設定をしています。どれも `etc/nginx` の変更で、アプリのコードは変えません。

| 設定 | ファイル | 何のため |
|---|---|---|
| `upstream` の `keepalive`（`keepalive_requests`、`keepalive_timeout` も） | `sites-available/isureal.conf`、`isureal-split.conf` | リクエストごとに Go のアプリへ TCP 接続を作って閉じるのをやめる。`proxy_http_version 1.1` と `proxy_set_header Connection ""` が一緒に要る |
| `worker_connections 8192`、`multi_accept on`、`worker_rlimit_nofile 16384` | `nginx.conf` | SSE は1本でクライアント側とアプリ側の2つの接続を使う。以前の 768 では、ワーカー1つあたり SSE 約380本で埋まり、超えると接続を受け付けなくなる |
| `/api/images/` と `/api/me/export` の `proxy_buffering off` | `isureal.conf`、`isureal-split.conf` | 画像（約450KB）と ZIP は既定の proxy バッファ（8 x 4k\|8k）に収まらず、残りを `/var/lib/nginx/proxy` の一時ファイルに書いていた。アプリが書いた分をそのまま流す。代案は `proxy_max_temp_file_size 0;` |

`isureal-split.conf` は asia2 のアプリに振り分ける構成（docs/asia-role-split.md）で asia1 に入れるもの。今は3台とも `isureal.conf` を使っている。どちらにも同じ設定を入れてあるので、切り替えても効果は変わらない。

## 反映

手元の repo のルートから、3台（asia1、europe1、america1）に配る。`sites-enabled/isureal.conf` のリンク先は変えない。

```bash
for h in asia1 europe1 america1; do
  echo "== $h"
  scp etc/nginx/nginx.conf $h:/tmp/nginx.conf
  scp etc/nginx/sites-available/isureal.conf $h:/tmp/isureal.conf
  scp etc/nginx/sites-available/isureal-split.conf $h:/tmp/isureal-split.conf
  ssh $h 'sudo cp /etc/nginx/nginx.conf /etc/nginx/nginx.conf.prev && sudo cp /etc/nginx/sites-available/isureal.conf /etc/nginx/sites-available/isureal.conf.prev
    && sudo install -m 644 /tmp/nginx.conf /etc/nginx/nginx.conf
    && sudo install -m 644 /tmp/isureal.conf /etc/nginx/sites-available/isureal.conf
    && sudo install -m 644 /tmp/isureal-split.conf /etc/nginx/sites-available/isureal-split.conf
    && sudo nginx -t && sudo systemctl reload nginx && systemctl is-active nginx'
done
```

`nginx -t` が通らなければ reload はされない（`&&` で止まる）。その場合は `.prev` を戻す。

### 元に戻す

```bash
for h in asia1 europe1 america1; do
  ssh $h 'sudo cp /etc/nginx/nginx.conf.prev /etc/nginx/nginx.conf && sudo cp /etc/nginx/sites-available/isureal.conf.prev /etc/nginx/sites-available/isureal.conf && sudo nginx -t && sudo systemctl reload nginx'
done
```

## 確かめ方

反映の前に1回、反映の後に1回、負荷走行をしてスコアを比べる。走行中に次を見る（どれも読み取りだけ）。

### keepalive が効いているか（asia1 で）

```bash
ssh asia1 'ss -tan state time-wait | wc -l; ss -tn dst 127.0.0.1:8080 | wc -l'
```

- TIME_WAIT が数千あれば、まだ接続を作って閉じている（`upstream` に向いていない location がないか確かめる）。
- `dst 127.0.0.1:8080` の数が、keepalive の数（64）程度で落ち着いていればよい。

### 接続数が足りているか（3台で）

```bash
for h in asia1 europe1 america1; do ssh $h "sudo grep -c 'worker_connections are not enough' /var/log/nginx/error.log"; done
```

0 でなければ、まだ足りない。`worker_connections` を上げる（`worker_rlimit_nofile` も一緒に）。

### 一時ファイルに書いていないか（3台で）

```bash
for h in asia1 europe1 america1; do ssh $h "sudo grep -c 'buffered to a temporary file' /var/log/nginx/error.log; sudo ls /var/lib/nginx/proxy/*/* 2>/dev/null | wc -l"; done
```

走行中に `pidstat -d 1` で nginx の書き込み（kB_wr/s）が 0 に近いことも確かめる。

### CPU（asia1 で）

```bash
ssh asia1 'pidstat -u 5 12 -C "nginx|isureal"'
```

nginx と isureal の %CPU を、反映の前後で比べる。

## 注意

- `/api/notifications` の `proxy_buffering off` と `proxy_read_timeout 1h` は、SSE に必要（#60）。消さない。
- gzip（#73）は `gzip_comp_level 1` で asia1 の CPU を使っている。keepalive の効果を見てから、asia1 だけ `gzip_types` を外すかを別に判断する。
- ルーターの設定には触れない。
