# ISUReal Ruby 実装

`webapp/go` と同じ API を Ruby で実装したものです。

- Ruby 4.0 / [Sinatra](https://sinatrarb.com/) / [Puma](https://puma.io/) / [pg](https://github.com/ged/ruby-pg)
- 依存 gem は `Gemfile` と `Gemfile.lock` で固定しています
- 画像の検証に libjpeg-turbo の TurboJPEG API を [ffi](https://github.com/ffi/ffi) から呼びます。実行には `libturbojpeg.so.0`（Debian では `libturbojpeg0`、Ubuntu では `libturbojpeg`）が必要です

## 構成

| パス | 内容 |
|---|---|
| `config.ru` | Rack アプリケーションの入口 |
| `config/puma.rb` | Puma の設定（待ち受けるポート、worker 数、スレッド数） |
| `lib/isureal/server.rb` | ルーティング |
| `lib/isureal/app.rb` | Sinatra アプリケーション本体（エラー処理、レスポンスの共通処理） |
| `lib/isureal/*_api.rb` | API ごとのハンドラ |
| `lib/isureal/*.rb` | ユーザー、投稿、セッションなどの DB の読み書きと共通処理 |
| `test/` | テスト |

ファイルの分け方とハンドラの名前は `webapp/go` に合わせています（例: `post_api.go` の
`createPostHandler` は `post_api.rb` の `create_post_handler`）。

`GET /api/notifications`（Server-Sent Events）は通知が届くまで接続を保持します。Puma のスレッドを
占有しないよう、レスポンスヘッダを送ったあとの接続は専用のスレッドが受け持ち、500ms ごとに DB を確認します。

## 起動

```sh
make install   # 依存 gem を vendor/bundle に入れる
make run       # Puma を起動し、:8080 で待ち受ける
```

## 環境変数

`webapp/go` と同じ `ISUREAL_REGION`、`ISUREAL_DB_HOST`、`ISUREAL_DB_PORT`、`ISUREAL_DB_USER`、
`ISUREAL_DB_PASSWORD`、`ISUREAL_DB_NAME`、`ISUREAL_LOG_LEVEL` を読みます。加えて次の値を変えられます。

| 変数 | 既定値 | 内容 |
|---|---|---|
| `ISUREAL_WORKERS` | `2` | Puma の worker（プロセス）数 |
| `ISUREAL_THREADS` | `16` | worker ごとのスレッド数 |
| `ISUREAL_DB_POOL_SIZE` | `15` | worker ごとの DB 接続プールの最大接続数 |

DB 接続は最大で `ISUREAL_WORKERS` × `ISUREAL_DB_POOL_SIZE` 本です。

## テスト

```sh
make test
```

DB を使うテストは `ISUREAL_TEST_DATABASE=1` のときだけ実行します。`ISUREAL_DB_*` で指定した DB の
テーブルを作り直すので、使い捨ての DB を指定してください。

```sh
ISUREAL_TEST_DATABASE=1 ISUREAL_DB_NAME=isureal_test make test
```
