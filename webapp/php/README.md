# ISUReal PHP 実装

`webapp/go` と同じ API を PHP で実装したものです。

- PHP 8.5 / [Slim](https://www.slimframework.com/) 4 / [PHP-DI](https://php-di.org/) / PDO（pdo_pgsql） / GD（JPEG の検証）
- php-fpm で動かし、nginx から FastCGI で受けます
- 依存パッケージは `composer.json` と `composer.lock` で固定しています

## 構成

| パス | 内容 |
|---|---|
| `public/index.php` | API の入口（Slim） |
| `public/notifications.php` | `GET /api/notifications`（Server-Sent Events）の入口 |
| `app/routes.php` | ルーティング |
| `app/dependencies.php` | DI コンテナの定義（設定と DB 接続） |
| `src/Handler/` | API ごとのハンドラ |
| `src/Repository/` | 複数のハンドラで使う DB の読み書き |
| `src/Model/` | ユーザー、投稿などのデータと応答の形 |
| `src/Http/` | エラー、リクエストの解釈、ミドルウェア |
| `src/Notification/NotificationStream.php` | 通知の配信 |
| `php-fpm.conf` | php-fpm の設定 |
| `tests/` | テスト |

`GET /api/notifications` は通知が届くまで接続を保持します。その間は php-fpm の worker を 1 つ使うので、
nginx はこのパスだけを別のプール（`notifications`、`127.0.0.1:9001`）に送ります。それ以外の API は
`www` プール（`127.0.0.1:9000`）が受けます。通知を待つ間は DB に接続したままにせず、500ms ごとに
接続して問い合わせます。

## 起動

```sh
make install   # 依存パッケージを vendor/ に入れる
make run       # php-fpm をフォアグラウンドで起動する
```

nginx の設定は、競技環境では `/etc/nginx/sites-available/isureal-php.conf`、開発環境では
`deploy/dev/nginx-php.conf` です。

## 環境変数

`webapp/go` と同じ `ISUREAL_REGION`、`ISUREAL_DB_HOST`、`ISUREAL_DB_PORT`、`ISUREAL_DB_USER`、
`ISUREAL_DB_PASSWORD`、`ISUREAL_DB_NAME`、`ISUREAL_LOG_LEVEL` を読みます（php-fpm の `clear_env = no` で worker に渡します）。

## テスト

```sh
make test   # 開発用のパッケージも入れてから PHPUnit を実行する
make lint   # PHPStan と PHP_CodeSniffer
```

DB を使うテスト（`tests/Integration`）は `ISUREAL_TEST_DATABASE=1` のときだけ実行します。`ISUREAL_DB_*` で
指定した DB のテーブルを作り直すので、使い捨ての DB を指定してください。同時リクエストと通知のテストは
`php -S` でサーバーを起動して確かめます。

```sh
ISUREAL_TEST_DATABASE=1 ISUREAL_DB_NAME=isureal_test make test
```
