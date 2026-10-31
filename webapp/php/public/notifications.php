<?php

declare(strict_types=1);

// GET /api/notifications（nginx がこのファイルに振り分ける）
require __DIR__ . '/../vendor/autoload.php';

use Isureal\Config;
use Isureal\Http\ActionLog;
use Isureal\Notification\NotificationStream;
use Slim\Factory\ServerRequestCreatorFactory;

$config = Config::fromEnv();
new NotificationStream($config, ActionLog::toStdout($config))->handle(ServerRequestCreatorFactory::create()->createServerRequestFromGlobals());
