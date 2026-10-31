# ISUReal Python 実装

`webapp/go` と同じ API を Python で実装したものです。

- Python 3.15 / [FastAPI](https://fastapi.tiangolo.com/)（gunicorn + uvicorn worker）/ [psycopg 3](https://www.psycopg.org/psycopg3/)
- パッケージ管理は [uv](https://docs.astral.sh/uv/) で、依存パッケージは `pyproject.toml` と `uv.lock` で固定しています

## 構成

| パス | 内容 |
|---|---|
| `gunicorn.conf.py` | gunicorn の設定（待ち受けるアドレス、worker 数） |
| `isureal/main.py` | アプリケーション本体（ルーターの登録、エラー処理、DB 接続プール） |
| `isureal/routers/*.py` | API ごとのハンドラ |
| `isureal/models/*.py` | ユーザー、投稿、セッションなどの DB の読み書き |
| `isureal/schemas.py` | API のレスポンス |
| `isureal/auth.py` | セッションとリージョンの確認（FastAPI の依存関係） |
| `isureal/binding.py` | リクエストボディとクエリの解釈 |
| `tests/` | テスト |

ハンドラはすべて `async def` で、DB へは非同期（psycopg の `AsyncConnection`）で問い合わせます。
`GET /api/notifications`（SSE）は通知が届くまで接続を保持しますが、待機中は worker を占有せず、
DB の接続もポーリングのたびに借りて返します。

## 起動

```sh
make install   # 依存パッケージを .venv に入れる
make run       # gunicorn を起動し、:8080 で待ち受ける
```

## 環境変数

`webapp/go` と同じ `ISUREAL_REGION`、`ISUREAL_DB_HOST`、`ISUREAL_DB_PORT`、`ISUREAL_DB_USER`、
`ISUREAL_DB_PASSWORD`、`ISUREAL_DB_NAME`、`ISUREAL_LOG_LEVEL` を読みます。加えて次の値を変えられます。

| 変数 | 既定値 | 内容 |
|---|---|---|
| `ISUREAL_WORKERS` | `4` | gunicorn の worker 数 |
| `ISUREAL_DB_POOL_SIZE` | `7` | worker ごとの DB 接続プールの最大接続数 |

DB 接続は最大で `ISUREAL_WORKERS` × `ISUREAL_DB_POOL_SIZE` 本です。

## テスト

```sh
uv sync        # 開発用のパッケージも入れる
make test
make lint      # ruff と mypy
```

DB を使うテストは `ISUREAL_TEST_DATABASE=1` のときだけ実行します。`ISUREAL_DB_*` で指定した DB の
テーブルを作り直すので、使い捨ての DB を指定してください。

```sh
ISUREAL_TEST_DATABASE=1 ISUREAL_DB_NAME=isureal_test make test
```
