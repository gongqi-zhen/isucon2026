# ISUReal Perl 実装

`webapp/go` と同じ API を Perl で実装したものです。

- Perl 5.44 / [Mojolicious](https://mojolicious.org/)（hypnotoad）/ [Mojo::Pg](https://metacpan.org/pod/Mojo::Pg)
- 依存モジュールは `cpanfile` と `cpanfile.snapshot` で固定しています
- 画像の検証に [Imager](https://metacpan.org/pod/Imager) を使います。`make install` の前に libjpeg の開発用パッケージ（Debian や Ubuntu では `libjpeg-dev`）を入れてください。入れずにビルドした Imager は JPEG を読めません

## 構成

| パス | 内容 |
|---|---|
| `script/isureal` | 起動スクリプト |
| `lib/Isureal.pm` | アプリケーション本体（設定、ルーティング、エラー処理） |
| `lib/Isureal/Controller/*.pm` | API ごとのハンドラ |
| `lib/Isureal/*.pm` | ユーザー、投稿、セッションなどの共通処理 |
| `t/` | テスト |

`GET /api/notifications`（SSE）は通知が届くまで接続を保持します。hypnotoad の各 worker は
イベントループで動くので、通知の待機中は DB への問い合わせを非同期（`Mojo::Pg` の `query_p`）で行い、
worker を占有しないようにしています。それ以外の API は同期的に DB へ問い合わせます。

## 起動

```sh
make install   # 依存モジュールを local/ に入れる
make run       # hypnotoad をフォアグラウンドで起動し、:8080 で待ち受ける
```

開発中は `perl script/isureal daemon -l 'http://*:8080'` でも起動できます（シングルプロセス）。

## 環境変数

`webapp/go` と同じ `ISUREAL_REGION`、`ISUREAL_DB_HOST`、`ISUREAL_DB_PORT`、`ISUREAL_DB_USER`、
`ISUREAL_DB_PASSWORD`、`ISUREAL_DB_NAME`、`ISUREAL_LOG_LEVEL` を読みます。加えて次の値を変えられます。

| 変数 | 既定値 | 内容 |
|---|---|---|
| `ISUREAL_PORT` | `8080` | 待ち受けるポート |
| `ISUREAL_WORKERS` | `8` | hypnotoad の worker 数 |
| `ISUREAL_ASYNC_DB_CONNECTIONS` | `2` | worker ごとの通知用（非同期クエリ用）の DB 接続数 |

DB 接続は worker ごとに同期クエリ用の 1 本と通知用の `ISUREAL_ASYNC_DB_CONNECTIONS` 本です。

## テスト

```sh
make test
```

DB を使うテストは `ISUREAL_TEST_DATABASE=1` のときだけ実行します。`ISUREAL_DB_*` で指定した DB の
テーブルを作り直すので、使い捨ての DB を指定してください。

```sh
ISUREAL_TEST_DATABASE=1 ISUREAL_DB_NAME=isureal_test make test
```
