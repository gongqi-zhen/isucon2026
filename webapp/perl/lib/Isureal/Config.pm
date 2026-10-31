package Isureal::Config;
use v5.44;
use utf8;

sub from_env ($class) {
    return bless {
        region     => env_or_default('ISUREAL_REGION',      'asia'),
        db_host    => env_or_default('ISUREAL_DB_HOST',     '127.0.0.1'),
        db_port    => env_or_default('ISUREAL_DB_PORT',     '5432'),
        db_user    => env_or_default('ISUREAL_DB_USER',     'isureal'),
        db_password => env_or_default('ISUREAL_DB_PASSWORD', 'isureal'),
        db_name    => env_or_default('ISUREAL_DB_NAME',     'isureal'),
        log_level  => env_or_default('ISUREAL_LOG_LEVEL',   'debug'),
        port       => env_or_default('ISUREAL_PORT',        '8080'),
        # hypnotoad の worker 数。各 worker は同期クエリ用と通知用の非同期クエリ用の DB 接続を持つ
        workers              => env_or_default('ISUREAL_WORKERS',              8),
        async_db_connections => env_or_default('ISUREAL_ASYNC_DB_CONNECTIONS', 2),
    }, $class;
}

sub database_dsn ($self) {
    return sprintf 'dbi:Pg:dbname=%s;host=%s;port=%s', $self->@{qw(db_name db_host db_port)};
}

sub env_or_default ($key, $default) {
    my $value = $ENV{$key};
    return defined $value && $value ne '' ? $value : $default;
}

1;
