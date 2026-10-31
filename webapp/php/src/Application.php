<?php

declare(strict_types=1);

namespace Isureal;

use DI\ContainerBuilder;
use Isureal\Http\ActionLog;
use Isureal\Http\ErrorHandler;
use Isureal\Http\Middleware\ActionLogMiddleware;
use Isureal\Http\Middleware\IpBanMiddleware;
use Isureal\Model\Region;
use Slim\App;
use Slim\Factory\AppFactory;

final class Application
{
    /**
     * @param array<string, mixed> $definitions app/dependencies.php を上書きする定義（テスト用）
     * @return App<\DI\Container>
     */
    public static function create(array $definitions = []): App
    {
        $builder = new ContainerBuilder();
        $builder->addDefinitions(__DIR__ . '/../app/dependencies.php', $definitions);
        $container = $builder->build();

        $config = $container->get(Config::class);
        if (!Region::isValid($config->region)) {
            throw new \RuntimeException("invalid server region: {$config->region}");
        }

        $app = AppFactory::createFromContainer($container);
        (require __DIR__ . '/../app/routes.php')($app);
        $app->addRoutingMiddleware();
        $app->add(IpBanMiddleware::class);
        $app->addErrorMiddleware(false, false, false)
            ->setDefaultErrorHandler(new ErrorHandler($app->getResponseFactory()));
        $app->add(new ActionLogMiddleware($app->getRouteResolver(), $container->get(ActionLog::class)));
        return $app;
    }
}
