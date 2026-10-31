<?php

declare(strict_types=1);

namespace Isureal\Handler;

use Isureal\Database;
use Isureal\Http\Json;
use Isureal\Http\Middleware\SessionMiddleware;
use Isureal\Model\PostingWindow;
use Isureal\Time;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class PostingWindowHandler
{
    public function __construct(private Database $db)
    {
    }

    /**
     * GET /api/posting-window/current
     */
    public function current(Request $request, Response $response): Response
    {
        $state = $this->db->fetchOne(
            'SELECT w.id, w.region, w.local_date, w.starts_at, w.on_time_deadline, w.ended_at,
                    count(p.id) FILTER (WHERE p.deleted_at IS NULL) AS active,
                    count(p.id) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
             FROM users u JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
             LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date
             LEFT JOIN posts p ON p.author_id = u.id AND p.posting_window_id = posted_window.id
             WHERE u.id = :user_id
             GROUP BY w.id',
            ['user_id' => SessionMiddleware::session($request)->userId],
        );
        if ($state === null) {
            return Json::respond($response, ['window' => null, 'hasPosted' => false, 'canPost' => false, 'deletionCount' => 0]);
        }
        $window = PostingWindow::fromRow($state);
        return Json::respond($response, [
            'window' => $window->toResponse(),
            'hasPosted' => $state['active'] > 0,
            'canPost' => $state['active'] === 0 && $state['deleted'] < 2 && Time::now() >= $window->startsAt,
            'deletionCount' => $state['deleted'],
        ]);
    }
}
