<?php

declare(strict_types=1);

namespace Isureal\Http;

use Isureal\Config;
use Isureal\Model\User;
use Psr\Http\Message\ServerRequestInterface;

final readonly class ActionLog
{
    private const array ACTION_NAMES = [
        'POST /api/initialize' => 'initialize',
        'POST /api/auth/register' => 'register',
        'POST /api/auth/login' => 'login',
        'POST /api/auth/logout' => 'logout',
        'GET /api/users' => 'search_users',
        'GET /api/users/:userId' => 'get_user',
        'GET /api/me/export' => 'export_posts',
        'GET /api/me' => 'get_me',
        'PATCH /api/me' => 'update_me',
        'PUT /api/me/profile-image' => 'put_profile_image',
        'DELETE /api/me/profile-image' => 'delete_profile_image',
        'POST /api/me/account-deletion' => 'delete_account',
        'POST /api/posts' => 'create_post',
        'GET /api/posting-window/current' => 'get_current_posting_window',
        'GET /api/posts/me/current' => 'get_my_current_post',
        'GET /api/posts/:postId' => 'get_post',
        'PATCH /api/posts/:postId' => 'update_post',
        'DELETE /api/posts/:postId' => 'delete_post',
        'GET /api/feeds/:feed' => 'get_feed',
        'GET /api/memories' => 'get_memories',
        'GET /api/memories/:date' => 'get_memories_by_date',
        'POST /api/posts/:postId/comments' => 'create_comment',
        'GET /api/posts/:postId/comments' => 'get_comments',
        'DELETE /api/comments/:commentId' => 'delete_comment',
        'POST /api/posts/:postId/realmojis' => 'replace_real_moji',
        'GET /api/posts/:postId/realmojis' => 'get_real_mojis',
        'DELETE /api/realmojis/:realMojiId' => 'delete_real_moji',
        'GET /api/images/:imageId' => 'get_image',
        'GET /api/notifications' => 'get_notifications',
        'POST /api/friend-requests' => 'create_friend_request',
        'POST /api/friend-requests/:requestId/:action' => 'transition_friend_request',
        'GET /api/relationships/:kind' => 'get_relationships',
        'DELETE /api/friendships/:userId' => 'delete_friendship',
        'PUT /api/follows/:userId' => 'follow',
        'DELETE /api/follows/:userId' => 'unfollow',
        'PUT /api/blocks/:userId' => 'block',
        'DELETE /api/blocks/:userId' => 'unblock',
        'POST /api/admin/auth/login' => 'admin_login',
        'POST /api/admin/auth/logout' => 'admin_logout',
        'PUT /api/admin/users/:userId/certification' => 'certify_user',
        'DELETE /api/admin/users/:userId/certification' => 'delete_user_certification',
        'POST /api/admin/posting-window' => 'open_posting_window',
    ];

    private const array LEVEL_RANKS = ['debug' => 0, 'info' => 1, 'error' => 2];

    /**
     * @param resource $output
     */
    public function __construct(private string $minLevel, private mixed $output)
    {
    }

    public static function toStdout(Config $config): self
    {
        return new self($config->logLevel, fopen('php://stdout', 'w') ?: throw new \RuntimeException('failed to open php://stdout'));
    }

    public function write(ServerRequestInterface $request, ?string $route, int $status, ActionLogEntry $entry): void
    {
        $method = $request->getMethod();
        $action = $route === null ? null : (self::ACTION_NAMES["{$method} {$route}"] ?? null);
        $level = match (true) {
            $status >= 500 => 'error',
            $method === 'GET' && !($status === 403 && $entry->abuse === 'invalid_signature') => 'debug',
            default => 'info',
        };
        if (self::LEVEL_RANKS[$level] < (self::LEVEL_RANKS[$this->minLevel] ?? 0)) {
            return;
        }
        $line = [
            'action' => $action,
            'ip' => ClientIp::of($request),
            'level' => $level,
            'status' => $status,
            'user_id' => $entry->userId,
        ];
        if ($action === 'login' || $action === 'register') {
            $line['username'] = $entry->username !== null && User::validUsername($entry->username) ? $entry->username : null;
        }
        if ($entry->abuse !== null) {
            $line['abuse'] = $entry->abuse;
        }
        fwrite($this->output, json_encode($line, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE | JSON_THROW_ON_ERROR) . "\n");
    }
}
