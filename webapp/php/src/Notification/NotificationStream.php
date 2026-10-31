<?php

declare(strict_types=1);

namespace Isureal\Notification;

use Isureal\Config;
use Isureal\Database;
use Isureal\Http\ActionLog;
use Isureal\Http\ActionLogEntry;
use Isureal\Http\ApiError;
use Isureal\Http\ClientIp;
use Isureal\Http\ErrorHandler;
use Isureal\Http\Json;
use Isureal\Http\Middleware\SessionMiddleware;
use Isureal\Model\LoginSession;
use Isureal\Repository\IpBanRepository;
use Isureal\Repository\SessionRepository;
use Isureal\Repository\UserRepository;
use Isureal\Time;
use Psr\Http\Message\ServerRequestInterface;
use Slim\Psr7\Response;
use Slim\ResponseEmitter;

/**
 * GET /api/notifications
 *
 * 新しい通知が届くまで接続を保持し、届いたら 1 件送って閉じる（Server-Sent Events）。
 * 待つ間は php-fpm の worker を占有するので、DB には問い合わせのたびに接続し、
 * 待つ間は接続を持たない。
 */
final class NotificationStream
{
    private const int POLL_INTERVAL_MICROSECONDS = 500_000;

    /**
     * 切断に気づけるよう、この回数だけ問い合わせるごとにコメント行を送る
     */
    private const int PING_EVERY_POLLS = 10;

    public function __construct(private readonly Config $config, private readonly ActionLog $log)
    {
    }

    public function handle(ServerRequestInterface $request): void
    {
        $entry = new ActionLogEntry();
        $request = $request->withAttribute(ActionLogEntry::ATTRIBUTE, $entry);
        $route = $request->getMethod() === 'GET' && $request->getUri()->getPath() === '/api/notifications' ? '/api/notifications' : null;
        try {
            [$session, $connectedAt] = $this->open($request);
        } catch (\Throwable $e) {
            $response = ErrorHandler::render(new Response(), $e);
            new ResponseEmitter()->emit($response);
            $this->log->write($request, $route, $response->getStatusCode(), $entry);
            return;
        }
        $this->stream($session, $connectedAt);
        $this->log->write($request, $route, 200, $entry);
    }

    /**
     * リクエストとセッションを確かめ、通知を探し始める時刻を返す
     *
     * @return array{LoginSession, string}
     */
    private function open(ServerRequestInterface $request): array
    {
        $db = Database::connect($this->config, persistent: false);
        if (new IpBanRepository($db)->isBanned(ClientIp::of($request))) {
            throw new ApiError(403, 'ip_banned');
        }
        if ($request->getMethod() !== 'GET') {
            throw new ApiError(405, 'Method Not Allowed');
        }
        if ($request->getUri()->getPath() !== '/api/notifications') {
            throw new ApiError(404, 'Not Found');
        }
        $session = new SessionMiddleware(new SessionRepository($db, new UserRepository($db)))->authenticate($request);
        if ($session->region !== $this->config->region) {
            throw new ApiError(403, 'region_mismatch');
        }
        return [$session, $db->fetchValue('SELECT clock_timestamp()')];
    }

    private function stream(LoginSession $session, string $connectedAt): void
    {
        set_time_limit(0);
        while (ob_get_level() > 0) {
            ob_end_flush();
        }
        http_response_code(200);
        // text/* に default_charset が付かないようにする
        ini_set('default_charset', '');
        header('Content-Type: text/event-stream');
        flush();

        for ($polls = 1; !connection_aborted(); $polls++) {
            try {
                $notification = $this->poll($session, $connectedAt);
            } catch (\Throwable $e) {
                error_log('failed to get notification: ' . $e);
                return;
            }
            if ($notification === false) {
                return;
            }
            if ($notification !== null) {
                echo 'data: ', Json::encode($notification), "\n\n";
                flush();
                return;
            }
            usleep(self::POLL_INTERVAL_MICROSECONDS);
            if ($polls % self::PING_EVERY_POLLS === 0) {
                echo ": ping\n\n";
                flush();
            }
        }
    }

    /**
     * 通知を 1 件探す。まだなければ null、通知を受けられないユーザーなら false
     *
     * @return array<string, string>|false|null
     */
    private function poll(LoginSession $session, string $connectedAt): array|false|null
    {
        $db = Database::connect($this->config, persistent: false);
        $user = new UserRepository($db)->find($session->userId);
        if ($user === null || $user->region !== $session->region) {
            return false;
        }
        $notification = $db->fetchOne(
            'SELECT n.id, n.posting_window_id, w.region, n.created_at
             FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
             WHERE n.user_id = :user_id AND n.created_at > :connected_at AND w.ended_at IS NULL
             ORDER BY n.created_at, n.id LIMIT 1',
            ['user_id' => $session->userId, 'connected_at' => $connectedAt],
        );
        if ($notification === null) {
            return null;
        }
        return [
            'id' => $notification['id'],
            'postingWindowId' => $notification['posting_window_id'],
            'region' => $notification['region'],
            'createdAt' => Time::format(Time::fromDb($notification['created_at'])),
        ];
    }
}
