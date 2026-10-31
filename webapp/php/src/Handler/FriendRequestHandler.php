<?php

declare(strict_types=1);

namespace Isureal\Handler;

use Isureal\Database;
use Isureal\Http\ApiError;
use Isureal\Http\Input;
use Isureal\Http\Json;
use Isureal\Http\Middleware\SessionMiddleware;
use Isureal\Ids;
use Isureal\Model\FriendRequest;
use Isureal\Repository\PostRepository;
use Isureal\Repository\RelationshipRepository;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class FriendRequestHandler
{
    public function __construct(
        private Database $db,
        private PostRepository $posts,
        private RelationshipRepository $relationships,
    ) {
    }

    /**
     * POST /api/friend-requests
     */
    public function create(Request $request, Response $response): Response
    {
        $recipientId = Input::json($request)->string('recipientId');
        $actor = SessionMiddleware::session($request)->userId;
        $friendRequest = $this->db->transaction(function (Database $db) use ($actor, $recipientId): FriendRequest {
            $this->relationships->lockPair($actor, $recipientId);
            if ($this->relationships->isBlocked($actor, $recipientId) || in_array($recipientId, $this->posts->friendIds($actor), true)) {
                throw new ApiError(409, 'relationship_conflict');
            }
            $existingId = $db->fetchValue(
                "SELECT id FROM friend_requests WHERE status = 'pending' AND
                  ((requester_id = :actor AND recipient_id = :recipient) OR (requester_id = :recipient AND recipient_id = :actor))",
                ['actor' => $actor, 'recipient' => $recipientId],
            );
            if ($existingId !== null) {
                // 相手から申請が来ていれば承認する
                $existing = $this->relationships->mustFindFriendRequest($existingId);
                if ($existing->requesterId === $actor) {
                    throw new ApiError(409, 'relationship_conflict');
                }
                $db->execute("UPDATE friend_requests SET status = 'accepted', updated_at = clock_timestamp() WHERE id = :id", ['id' => $existing->id]);
                $this->relationships->insertFriendship($existing);
                $requestId = $existing->id;
            } else {
                $requestId = Ids::generate();
                $db->execute(
                    "INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
                     VALUES (:id, :requester_id, :recipient_id, 'pending', clock_timestamp(), clock_timestamp())",
                    ['id' => $requestId, 'requester_id' => $actor, 'recipient_id' => $recipientId],
                );
            }
            return $this->relationships->mustFindFriendRequest($requestId);
        });
        return Json::respond($response, $friendRequest->toResponse(), 201);
    }

    /**
     * POST /api/friend-requests/{requestId}/{action}
     *
     * @param array<string, string> $args
     */
    public function transition(Request $request, Response $response, array $args): Response
    {
        $status = match ($args['action']) {
            'accept' => 'accepted',
            'reject' => 'rejected',
            'cancel' => 'cancelled',
            default => throw new ApiError(403, 'relationship_action_forbidden'),
        };
        $actor = SessionMiddleware::session($request)->userId;
        $friendRequest = $this->relationships->findFriendRequest($args['requestId']) ?? throw new ApiError(404, 'relationship_not_found');
        $allowedActor = $status === 'cancelled' ? $friendRequest->requesterId : $friendRequest->recipientId;
        if ($actor !== $allowedActor) {
            throw new ApiError(403, 'relationship_action_forbidden');
        }
        $friendRequest = $this->db->transaction(function (Database $db) use ($friendRequest, $status): FriendRequest {
            $this->relationships->lockPair($friendRequest->requesterId, $friendRequest->recipientId);
            $friendRequest = $this->relationships->mustFindFriendRequest($friendRequest->id);
            if ($friendRequest->status !== 'pending') {
                throw new ApiError(403, 'relationship_action_forbidden');
            }
            if ($status === 'accepted') {
                if ($this->relationships->isBlocked($friendRequest->requesterId, $friendRequest->recipientId)) {
                    throw new ApiError(409, 'relationship_conflict');
                }
                $this->relationships->insertFriendship($friendRequest);
            }
            $db->execute('UPDATE friend_requests SET status = :status, updated_at = clock_timestamp() WHERE id = :id', ['id' => $friendRequest->id, 'status' => $status]);
            return $this->relationships->mustFindFriendRequest($friendRequest->id);
        });
        return Json::respond($response, $friendRequest->toResponse());
    }
}
