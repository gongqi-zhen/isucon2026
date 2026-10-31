.PHONY: install run test lint

# 依存パッケージを composer.lock のとおりに vendor/ へ入れる
install:
	composer install --no-dev --optimize-autoloader --no-interaction

# php-fpm をフォアグラウンドで起動する（nginx が :8080 で受けて FastCGI で渡す）
run:
	ISUREAL_PHP_FPM_ERROR_LOG=/proc/self/fd/2 php-fpm --nodaemonize --fpm-config php-fpm.conf

test:
	composer install --no-interaction
	vendor/bin/phpunit

lint:
	vendor/bin/phpstan analyse --memory-limit=1G
	vendor/bin/phpcs
