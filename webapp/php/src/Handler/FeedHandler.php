<?php

declare(strict_types=1);

namespace Isureal\Handler;

use DateTimeImmutable;
use Isureal\Database;
use Isureal\Http\ApiError;
use Isureal\Http\Input;
use Isureal\Http\Json;
use Isureal\Http\Middleware\SessionMiddleware;
use Isureal\Model\Post;
use Isureal\Model\User;
use Isureal\Model\ViewerContext;
use Isureal\Model\Visibility;
use Isureal\Repository\PostRepository;
use Isureal\Time;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class FeedHandler
{
    private const array FEEDS = ['friends', 'friends-of-friends', 'public', 'following'];

    public function __construct(private Database $db, private PostRepository $posts)
    {
    }

    /**
     * GET /api/feeds/{feed}
     *
     * @param array<string, string> $args
     */
    public function show(Request $request, Response $response, array $args): Response
    {
        $feed = $args['feed'];
        $beforeScore = Input::queryInt($request, 'beforeScore');
        $beforePostedAt = Input::queryTime($request, 'beforePostedAt');
        $beforeId = Input::queryString($request, 'beforeId');
        if (!in_array($feed, self::FEEDS, true)) {
            throw new ApiError(400, 'invalid_post');
        }
        if (($beforePostedAt === null) !== ($beforeId === '')) {
            throw new ApiError(400, 'invalid_pagination');
        }
        $ranked = $feed === 'friends-of-friends';
        if ($ranked && ($beforeScore === null) !== ($beforeId === '')) {
            throw new ApiError(400, 'invalid_pagination');
        }
        $viewer = $this->posts->loadViewer(SessionMiddleware::session($request)->userId);
        if ($viewer === null) {
            return Json::respond($response, ['items' => [], 'hasMore' => false]);
        }
        $following = [];
        if ($feed === 'following') {
            $followeeIds = $this->db->fetchColumn('SELECT followee_id FROM follows WHERE follower_id = :follower_id', ['follower_id' => $viewer->user->id]);
            $following = array_fill_keys($followeeIds, true);
        }
        $posts = $this->posts->selectWithAuthor(
            'AND p.author_id <> :viewer_id AND w.ended_at IS NULL
             AND (:before_posted_at::timestamptz IS NULL OR (p.posted_at, p.id) < (:before_posted_at, :before_id))
             ORDER BY p.posted_at DESC, p.id DESC',
            [
                'viewer_id' => $viewer->user->id,
                'before_posted_at' => $ranked || $beforePostedAt === null ? null : Time::toDb($beforePostedAt),
                'before_id' => $ranked ? '' : $beforeId,
            ],
        );
        $candidates = array_values(array_filter(
            $posts,
            static fn (array $post): bool => $viewer->canView($post[0]) && self::matches($viewer, $following, $post[0], $feed),
        ));
        $scores = [];
        if ($ranked) {
            $scores = $this->recommendationScores($viewer, $candidates);
            usort($candidates, static fn (array $a, array $b): int => $scores[$b[0]->id] <=> $scores[$a[0]->id]);
        }
        $items = [];
        foreach ($candidates as [$post, $author]) {
            $score = $scores[$post->id] ?? 0;
            if ($ranked && $beforeScore !== null && $beforePostedAt !== null && !self::rankedBefore($score, $post, $beforeScore, $beforePostedAt, $beforeId)) {
                continue;
            }
            $item = $post->toResponse($author);
            if ($ranked) {
                $item['score'] = $score;
            }
            $items[] = $item;
            if (count($items) === 51) {
                break;
            }
        }
        return Json::respond($response, UserHandler::page($items));
    }

    private static function rankedBefore(int $score, Post $post, int $beforeScore, DateTimeImmutable $beforePostedAt, string $beforeId): bool
    {
        if ($score !== $beforeScore) {
            return $score < $beforeScore;
        }
        if ($post->postedAt != $beforePostedAt) {
            return $post->postedAt < $beforePostedAt;
        }
        return strcmp($post->id, $beforeId) < 0;
    }

    /**
     * @param list<array{Post, User}> $posts
     * @return array<string, int>
     */
    private function recommendationScores(ViewerContext $viewer, array $posts): array
    {
        $scores = [];
        if ($posts === []) {
            return $scores;
        }
        $authorIds = array_map(static fn (array $post): string => $post[0]->authorId, $posts);
        $postIds = array_map(static fn (array $post): string => $post[0]->id, $posts);

        [$in, $params] = Database::inClause('author', $authorIds);
        $friendships = $this->db->fetchAll(
            "SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN ({$in}) OR user_high_id IN ({$in})",
            $params,
        );
        $connections = [];
        foreach ($friendships as ['user_low_id' => $low, 'user_high_id' => $high]) {
            $connections[$low] = ($connections[$low] ?? 0) + ($viewer->mutualFriends[$high] ?? 0);
            $connections[$high] = ($connections[$high] ?? 0) + ($viewer->mutualFriends[$low] ?? 0);
        }

        [$in, $params] = Database::inClause('post', $postIds);
        $engagements = $this->db->fetchAll(
            "SELECT r.post_id, r.reactor_id AS user_id FROM realmojis r
             JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
             WHERE r.post_id IN ({$in}) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
             UNION
             SELECT c.post_id, c.author_id AS user_id FROM comments c
             JOIN users u ON u.id = c.author_id AND u.status = 'active'
             WHERE c.post_id IN ({$in}) AND c.deleted_at IS NULL",
            $params,
        );
        $friendEngagements = [];
        foreach ($engagements as ['post_id' => $postId, 'user_id' => $userId]) {
            if ($viewer->isFriend($userId)) {
                $friendEngagements[$postId] = ($friendEngagements[$postId] ?? 0) + 1;
            }
        }

        foreach ($posts as [$post]) {
            $scores[$post->id] = 10 * ($friendEngagements[$post->id] ?? 0) + 3 * ($viewer->mutualFriends[$post->authorId] ?? 0) + min($connections[$post->authorId] ?? 0, 30);
        }
        return $scores;
    }

    /**
     * @param array<string, true> $following
     */
    private static function matches(ViewerContext $viewer, array $following, Post $post, string $feed): bool
    {
        return match ($feed) {
            'public' => $post->visibility === Visibility::Public->value,
            'following' => isset($following[$post->authorId]),
            'friends' => $viewer->isFriend($post->authorId),
            'friends-of-friends' => !$viewer->isFriend($post->authorId) && $viewer->isFriendOfFriend($post->authorId),
            default => false,
        };
    }
}
