<?php

declare(strict_types=1);

namespace Isureal\Handler;

use Isureal\Database;
use Isureal\Http\ApiError;
use Isureal\Http\Input;
use Isureal\Http\Json;
use Isureal\Http\Middleware\SessionMiddleware;
use Isureal\Ids;
use Isureal\Model\User;
use Isureal\Repository\RelationshipRepository;
use Isureal\Repository\UserRepository;
use Isureal\Time;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class RelationshipHandler
{
    /**
     * 一覧の種類ごとの関係。first_id と second_id のどちらかが自分（:actor）
     */
    private const array RELATIONS = [
        'friends' => 'SELECT id, user_low_id AS first_id, user_high_id AS second_id, updated_at FROM friendships WHERE user_low_id = :actor OR user_high_id = :actor',
        'following' => 'SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE follower_id = :actor',
        'followers' => 'SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE followee_id = :actor',
        'incoming-requests' => "SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE recipient_id = :actor AND status = 'pending'",
        'outgoing-requests' => "SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE requester_id = :actor AND status = 'pending'",
    ];

    public function __construct(
        private Database $db,
        private UserRepository $users,
        private RelationshipRepository $relationships,
    ) {
    }

    /**
     * GET /api/relationships/{kind}
     *
     * @param array<string, string> $args
     */
    public function list(Request $request, Response $response, array $args): Response
    {
        $kind = $args['kind'];
        $beforeUpdatedAt = Input::queryTime($request, 'beforeUpdatedAt');
        $beforeId = Input::queryString($request, 'beforeId');
        if (($beforeUpdatedAt === null) !== ($beforeId === '')) {
            throw new ApiError(400, 'invalid_pagination');
        }
        $relation = self::RELATIONS[$kind] ?? throw new ApiError(400, 'invalid_relationship');
        $rows = $this->db->fetchAll(
            'SELECT relations.id, relations.updated_at, ' . User::columnsAs('u', 'user') . "
             FROM ({$relation}) relations
             JOIN users u ON u.status = 'active'
                         AND u.id = CASE WHEN relations.first_id = :actor THEN relations.second_id ELSE relations.first_id END
             WHERE (:before_updated_at::timestamptz IS NULL OR (relations.updated_at, relations.id) < (:before_updated_at, :before_id))
             ORDER BY relations.updated_at DESC, relations.id DESC LIMIT 51",
            [
                'actor' => SessionMiddleware::session($request)->userId,
                'before_updated_at' => $beforeUpdatedAt === null ? null : Time::toDb($beforeUpdatedAt),
                'before_id' => $beforeId,
            ],
        );
        $status = str_ends_with($kind, '-requests') ? 'pending' : $kind;
        $items = [];
        foreach ($rows as $row) {
            $user = User::fromRow($row, 'user.');
            if (!$user->isCertified() && $kind !== 'friends') {
                $user->bio = '';
            }
            $items[] = [
                'relationId' => $row['id'],
                'user' => $user->toPublic(),
                'status' => $status,
                'updatedAt' => Time::format(Time::fromDb($row['updated_at'])),
            ];
        }
        return Json::respond($response, UserHandler::page($items));
    }

    /**
     * DELETE /api/friendships/{userId}
     *
     * @param array<string, string> $args
     */
    public function deleteFriendship(Request $request, Response $response, array $args): Response
    {
        $this->change($request, $args['userId'], function (Database $db, string $actor, string $target): void {
            [$low, $high] = Ids::ordered($actor, $target);
            $deleted = $db->execute('DELETE FROM friendships WHERE user_low_id = :low AND user_high_id = :high', ['low' => $low, 'high' => $high]);
            if ($deleted === 0) {
                throw new ApiError(409, 'relationship_conflict');
            }
        });
        return Json::noContent($response);
    }

    /**
     * PUT /api/follows/{userId}
     *
     * @param array<string, string> $args
     */
    public function follow(Request $request, Response $response, array $args): Response
    {
        $this->change($request, $args['userId'], function (Database $db, string $actor, string $target): void {
            $user = $this->users->mustFind($target);
            if (!$user->isCertified() || $this->relationships->isBlocked($actor, $user->id)) {
                throw new ApiError(409, 'relationship_conflict');
            }
            $exists = $db->fetchValue(
                'SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id = :follower_id AND followee_id = :followee_id)',
                ['follower_id' => $actor, 'followee_id' => $user->id],
            );
            if ($exists) {
                throw new ApiError(409, 'relationship_conflict');
            }
            $db->execute(
                'INSERT INTO follows (id, follower_id, followee_id) VALUES (:id, :follower_id, :followee_id)',
                ['id' => Ids::generate(), 'follower_id' => $actor, 'followee_id' => $user->id],
            );
        });
        return Json::noContent($response);
    }

    /**
     * DELETE /api/follows/{userId}
     *
     * @param array<string, string> $args
     */
    public function unfollow(Request $request, Response $response, array $args): Response
    {
        $this->change($request, $args['userId'], function (Database $db, string $actor, string $target): void {
            $deleted = $db->execute('DELETE FROM follows WHERE follower_id = :follower_id AND followee_id = :followee_id', ['follower_id' => $actor, 'followee_id' => $target]);
            if ($deleted === 0) {
                throw new ApiError(409, 'relationship_conflict');
            }
        });
        return Json::noContent($response);
    }

    /**
     * PUT /api/blocks/{userId}
     *
     * @param array<string, string> $args
     */
    public function block(Request $request, Response $response, array $args): Response
    {
        $this->change($request, $args['userId'], function (Database $db, string $actor, string $target): void {
            $exists = $db->fetchValue(
                'SELECT EXISTS(SELECT 1 FROM block_relations WHERE blocker_id = :actor AND blocked_id = :target)',
                ['actor' => $actor, 'target' => $target],
            );
            if ($exists) {
                throw new ApiError(409, 'relationship_conflict');
            }
            $db->execute(
                'INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES (:id, :actor, :target)',
                ['id' => Ids::generate(), 'actor' => $actor, 'target' => $target],
            );
            [$low, $high] = Ids::ordered($actor, $target);
            $db->execute('DELETE FROM friendships WHERE user_low_id = :low AND user_high_id = :high', ['low' => $low, 'high' => $high]);
            $db->execute(
                'DELETE FROM follows WHERE (follower_id = :actor AND followee_id = :target) OR (follower_id = :target AND followee_id = :actor)',
                ['actor' => $actor, 'target' => $target],
            );
            $db->execute(
                "UPDATE friend_requests SET status = 'cancelled', updated_at = clock_timestamp()
                 WHERE status = 'pending' AND ((requester_id = :actor AND recipient_id = :target) OR (requester_id = :target AND recipient_id = :actor))",
                ['actor' => $actor, 'target' => $target],
            );
        });
        return Json::noContent($response);
    }

    /**
     * DELETE /api/blocks/{userId}
     *
     * @param array<string, string> $args
     */
    public function unblock(Request $request, Response $response, array $args): Response
    {
        $this->change($request, $args['userId'], function (Database $db, string $actor, string $target): void {
            $deleted = $db->execute('DELETE FROM block_relations WHERE blocker_id = :actor AND blocked_id = :target', ['actor' => $actor, 'target' => $target]);
            if ($deleted === 0) {
                throw new ApiError(403, 'relationship_action_forbidden');
            }
        });
        return Json::noContent($response);
    }

    /**
     * 2 人のユーザーをロックしてから関係を変える
     *
     * @param callable(Database, string, string): void $change 引数は DB、自分、相手
     */
    private function change(Request $request, string $target, callable $change): void
    {
        $actor = SessionMiddleware::session($request)->userId;
        $this->db->transaction(function (Database $db) use ($actor, $target, $change): void {
            $this->relationships->lockPair($actor, $target);
            $change($db, $actor, $target);
        });
    }
}
