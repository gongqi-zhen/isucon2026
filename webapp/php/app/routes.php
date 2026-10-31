<?php

declare(strict_types=1);

use Isureal\Handler\AdminHandler;
use Isureal\Handler\AuthHandler;
use Isureal\Handler\CommentHandler;
use Isureal\Handler\ExportHandler;
use Isureal\Handler\FeedHandler;
use Isureal\Handler\FriendRequestHandler;
use Isureal\Handler\ImageHandler;
use Isureal\Handler\InitializeHandler;
use Isureal\Handler\MeHandler;
use Isureal\Handler\MemoryHandler;
use Isureal\Handler\PostHandler;
use Isureal\Handler\PostingWindowHandler;
use Isureal\Handler\RealMojiHandler;
use Isureal\Handler\RelationshipHandler;
use Isureal\Handler\UserHandler;
use Isureal\Http\Middleware\AdminSessionMiddleware;
use Isureal\Http\Middleware\RegionMiddleware;
use Isureal\Http\Middleware\SessionMiddleware;
use Slim\App;
use Slim\Routing\RouteCollectorProxy;

// GET /api/notifications は public/notifications.php が受ける（nginx で振り分ける）
return function (App $app): void {
    $app->group('/api', function (RouteCollectorProxy $api): void {
        $api->post('/initialize', InitializeHandler::class);
        $api->post('/auth/register', [AuthHandler::class, 'register']);
        $api->post('/auth/login', [AuthHandler::class, 'login']);
        $api->get('/images/{imageId}', [ImageHandler::class, 'show']);
        $api->post('/admin/auth/login', [AdminHandler::class, 'login']);
    });

    // ログインが必要
    $app->group('/api', function (RouteCollectorProxy $api): void {
        $api->post('/auth/logout', [AuthHandler::class, 'logout']);
        $api->patch('/me', [MeHandler::class, 'update']);
        $api->put('/me/profile-image', [MeHandler::class, 'putProfileImage']);
        $api->delete('/me/profile-image', [MeHandler::class, 'deleteProfileImage']);
    })->add(SessionMiddleware::class);

    // ログインが必要で、このサーバーのリージョンのユーザーだけが使える
    $app->group('/api', function (RouteCollectorProxy $api): void {
        $api->get('/users', [UserHandler::class, 'search']);
        $api->get('/users/{userId}', [UserHandler::class, 'show']);
        $api->get('/me/export', [ExportHandler::class, 'export']);
        $api->get('/me', [MeHandler::class, 'show']);
        $api->post('/me/account-deletion', [MeHandler::class, 'deleteAccount']);
        $api->post('/posts', [PostHandler::class, 'create']);
        $api->get('/posting-window/current', [PostingWindowHandler::class, 'current']);
        $api->get('/posts/me/current', [PostHandler::class, 'myCurrent']);
        $api->get('/posts/{postId}', [PostHandler::class, 'show']);
        $api->patch('/posts/{postId}', [PostHandler::class, 'update']);
        $api->delete('/posts/{postId}', [PostHandler::class, 'delete']);
        $api->get('/feeds/{feed}', [FeedHandler::class, 'show']);
        $api->get('/memories', [MemoryHandler::class, 'byMonth']);
        $api->get('/memories/{date}', [MemoryHandler::class, 'byDate']);
        $api->post('/posts/{postId}/comments', [CommentHandler::class, 'create']);
        $api->get('/posts/{postId}/comments', [CommentHandler::class, 'list']);
        $api->delete('/comments/{commentId}', [CommentHandler::class, 'delete']);
        $api->post('/posts/{postId}/realmojis', [RealMojiHandler::class, 'replace']);
        $api->get('/posts/{postId}/realmojis', [RealMojiHandler::class, 'list']);
        $api->delete('/realmojis/{realMojiId}', [RealMojiHandler::class, 'delete']);
        $api->post('/friend-requests', [FriendRequestHandler::class, 'create']);
        $api->post('/friend-requests/{requestId}/{action}', [FriendRequestHandler::class, 'transition']);
        $api->get('/relationships/{kind}', [RelationshipHandler::class, 'list']);
        $api->delete('/friendships/{userId}', [RelationshipHandler::class, 'deleteFriendship']);
        $api->put('/follows/{userId}', [RelationshipHandler::class, 'follow']);
        $api->delete('/follows/{userId}', [RelationshipHandler::class, 'unfollow']);
        $api->put('/blocks/{userId}', [RelationshipHandler::class, 'block']);
        $api->delete('/blocks/{userId}', [RelationshipHandler::class, 'unblock']);
    })->add(RegionMiddleware::class)->add(SessionMiddleware::class);

    // 管理者
    $app->group('/api/admin', function (RouteCollectorProxy $admin): void {
        $admin->post('/auth/logout', [AdminHandler::class, 'logout']);
        $admin->put('/users/{userId}/certification', [AdminHandler::class, 'certifyUser']);
        $admin->delete('/users/{userId}/certification', [AdminHandler::class, 'deleteUserCertification']);
        $admin->post('/posting-window', [AdminHandler::class, 'openPostingWindow']);
    })->add(AdminSessionMiddleware::class);
};
