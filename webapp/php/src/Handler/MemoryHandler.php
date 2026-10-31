<?php

declare(strict_types=1);

namespace Isureal\Handler;

use DateTimeImmutable;
use Isureal\Database;
use Isureal\Http\ApiError;
use Isureal\Http\Input;
use Isureal\Http\Json;
use Isureal\Http\Middleware\SessionMiddleware;
use Isureal\Repository\CommentRepository;
use Isureal\Repository\PostRepository;
use Isureal\Repository\RealMojiRepository;
use Isureal\Time;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class MemoryHandler
{
    public function __construct(
        private PostRepository $posts,
        private RealMojiRepository $realMojis,
        private CommentRepository $comments,
    ) {
    }

    /**
     * GET /api/memories
     */
    public function byMonth(Request $request, Response $response): Response
    {
        $month = Input::queryString($request, 'month');
        $beforePostedAt = Input::queryTime($request, 'beforePostedAt');
        $start = preg_match('/\A\d{4}-\d\d\z/', $month) ? Time::parseDate("{$month}-01") : null;
        if ($start === null) {
            throw new ApiError(400, 'invalid_memory_period');
        }
        return Json::respond($response, $this->list($request, $start, $start->modify('+1 month'), $beforePostedAt));
    }

    /**
     * GET /api/memories/{date}
     *
     * @param array<string, string> $args
     */
    public function byDate(Request $request, Response $response, array $args): Response
    {
        $beforePostedAt = Input::queryTime($request, 'beforePostedAt');
        $start = Time::parseDate($args['date']) ?? throw new ApiError(400, 'invalid_memory_period');
        return Json::respond($response, $this->list($request, $start, $start->modify('+1 day'), $beforePostedAt));
    }

    /**
     * @return array{items: list<array<string, mixed>>, hasMore: bool}
     */
    private function list(Request $request, DateTimeImmutable $start, DateTimeImmutable $end, ?DateTimeImmutable $beforeAt): array
    {
        $beforeId = Input::queryString($request, 'beforeId');
        if (($beforeAt === null) !== ($beforeId === '')) {
            throw new ApiError(400, 'invalid_pagination');
        }
        $posts = $this->posts->selectWithAuthor(
            'AND p.author_id = :author_id AND w.local_date >= :start AND w.local_date < :end
             AND (:before_at::timestamptz IS NULL OR (p.posted_at, p.id) < (:before_at, :before_id))
             ORDER BY p.posted_at DESC, p.id DESC LIMIT 51',
            [
                'author_id' => SessionMiddleware::session($request)->userId,
                'start' => $start->format('Y-m-d'),
                'end' => $end->format('Y-m-d'),
                'before_at' => $beforeAt === null ? null : Time::toDb($beforeAt),
                'before_id' => $beforeId,
            ],
        );
        $items = [];
        foreach ($posts as [$post, $author]) {
            $items[$post->id] = [...$post->toResponse($author), 'realmojis' => [], 'comments' => []];
        }
        $hasMore = count($items) > 50;
        $items = array_slice($items, 0, 50, preserve_keys: true);
        $this->loadInteractions($items);
        return ['items' => array_values($items), 'hasMore' => $hasMore];
    }

    /**
     * 投稿ごとに RealMoji とコメントを 50 件まで入れる
     *
     * @param array<string, array<string, mixed>> $items 投稿の ID をキーにした投稿
     */
    private function loadInteractions(array &$items): void
    {
        if ($items === []) {
            return;
        }
        [$in, $params] = Database::inClause('post', array_map(strval(...), array_keys($items)));

        $reactions = $this->realMojis->selectCurrent("AND r.post_id IN ({$in}) ORDER BY r.created_at DESC, r.id DESC", $params);
        foreach ($reactions as [$reaction, $reactor]) {
            if (count($items[$reaction->postId]['realmojis']) < 50) {
                $items[$reaction->postId]['realmojis'][] = $reaction->toResponse($reactor);
            }
        }

        $comments = $this->comments->selectWithAuthor("AND c.post_id IN ({$in}) ORDER BY c.created_at, c.id", $params);
        foreach ($comments as [$comment, $author]) {
            if (count($items[$comment->postId]['comments']) < 50) {
                $items[$comment->postId]['comments'][] = $comment->toResponse($author);
            }
        }
    }
}
