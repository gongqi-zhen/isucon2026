<?php

declare(strict_types=1);

use Isureal\Config;
use Isureal\Database;
use Isureal\Http\ActionLog;
use Psr\Container\ContainerInterface;

// ここにないクラスは PHP-DI がコンストラクタの型から組み立てる
return [
    Config::class => static fn (): Config => Config::fromEnv(),
    Database::class => static fn (ContainerInterface $c): Database => Database::connect($c->get(Config::class)),
    ActionLog::class => static fn (ContainerInterface $c): ActionLog => ActionLog::toStdout($c->get(Config::class)),
];
