# ISUReal Rust 実装

`webapp/go` と同じ API を Rust で実装したものです。

- Rust 1.99 / [axum](https://github.com/tokio-rs/axum) 0.8 / [sqlx](https://github.com/launchbadge/sqlx) 0.8 / [tokio](https://tokio.rs/)
- Rust のバージョンは `rust-toolchain.toml`、依存クレートは `Cargo.toml` と `Cargo.lock` で固定しています

## 構成

ファイルの分け方、関数と構造体の名前、SQL、トランザクションとロックの順序は `webapp/go` に揃えています。
`webapp/go` の `xxx.go` の処理は `src/xxx.rs` にあります。

| パス | 内容 |
|---|---|
| `src/main.rs` | 起動（`webapp/go/cmd/isureal/main.go` に当たる） |
| `src/server.rs` | ルーティング、ミドルウェア、DB 接続プール |
| `src/*_api.rs` | API ごとのハンドラ |
| `src/bind.rs` | パスパラメータ、クエリ、リクエストボディの解釈（Echo の `Bind` に当たる） |
| `src/api_error.rs`、`src/response.rs` | エラーとレスポンスの組み立て |
| `src/db.rs` | SQL の引数の組み立てと `numeric` の変換 |
| `src/jpeg.rs` | アップロードされた JPEG の検証 |
| `src/timefmt.rs` | 日時の解析と JSON の形式 |
| `tests/` | テスト |

## 起動

```sh
make        # ビルドして isureal を作る
./isureal   # :8080 で待ち受ける
```

`/api/initialize` は `../sql/init.sh` を実行するので、このディレクトリで起動してください。

## 環境変数

`webapp/go` と同じ `ISUREAL_REGION`、`ISUREAL_DB_HOST`、`ISUREAL_DB_PORT`、`ISUREAL_DB_USER`、
`ISUREAL_DB_PASSWORD`、`ISUREAL_DB_NAME`、`ISUREAL_LOG_LEVEL` を読みます。DB の接続は最大 30 本です。

## テスト

```sh
make test
make lint   # rustfmt と clippy
```
