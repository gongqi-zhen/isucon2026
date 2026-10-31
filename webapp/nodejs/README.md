# ISUReal Node.js 実装

`webapp/go` と同じ API を Node.js で実装したものです。

- Node.js 26 / TypeScript / [Hono](https://hono.dev/) / [node-postgres](https://node-postgres.com/)
- 依存パッケージは `package.json` と `package-lock.json` で固定しています
- TypeScript はビルドせず、Node.js の型の除去（type stripping）でそのまま実行します

## 構成

| パス | 内容 |
|---|---|
| `src/main.ts` | 入口。`node:cluster` で worker を起動し、`:8080` で待ち受ける |
| `src/server.ts` | ルーティング |
| `src/*_api.ts` | API ごとのハンドラ |
| `src/*.ts` | ユーザー、投稿、セッションなどの DB の読み書きと共通処理 |
| `test/` | テスト |

ファイルの分け方とハンドラの名前は `webapp/go` に合わせています（例: `post_api.go` の
`createPostHandler` は `post_api.ts` の `createPostHandler`）。

## 起動

```sh
npm ci      # 依存パッケージを node_modules に入れる
npm start   # :8080 で待ち受ける
npm run check   # 型を検査する（tsc）
```

## 環境変数

`webapp/go` と同じ `ISUREAL_REGION`、`ISUREAL_DB_HOST`、`ISUREAL_DB_PORT`、`ISUREAL_DB_USER`、
`ISUREAL_DB_PASSWORD`、`ISUREAL_DB_NAME`、`ISUREAL_LOG_LEVEL` を読みます。加えて次の値を変えられます。

| 変数 | 既定値 | 内容 |
|---|---|---|
| `ISUREAL_WORKERS` | `2` | worker（プロセス）数 |
| `ISUREAL_DB_POOL_SIZE` | `15` | worker ごとの DB 接続プールの最大接続数 |

DB 接続は最大で `ISUREAL_WORKERS` × `ISUREAL_DB_POOL_SIZE` 本です。

## テスト

```sh
npm test
```

DB を使うテストは `ISUREAL_TEST_DATABASE=1` のときだけ実行します。`ISUREAL_DB_*` で指定した DB の
テーブルを作り直すので、使い捨ての DB を指定してください。

```sh
ISUREAL_TEST_DATABASE=1 ISUREAL_DB_NAME=isureal_test npm test
```
