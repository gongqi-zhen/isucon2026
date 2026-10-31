<?php

declare(strict_types=1);

namespace Isureal\Repository;

use Isureal\Database;
use Isureal\Http\ApiError;
use Isureal\Ids;
use Isureal\Model\FriendRequest;

final readonly class RelationshipRepository
{
    public function __construct(private Database $db)
    {
    }

    /**
     * 関係を変える 2 人のユーザーを ID の順にロックする。トランザクションの中で呼ぶ
     */
    public function lockPair(string $actor, string $target): void
    {
        if ($actor === $target || $target === '') {
            throw new ApiError(400, 'invalid_relationship');
        }
        foreach (Ids::ordered($actor, $target) as $id) {
            $locked = $this->db->fetchValue("SELECT id FROM users WHERE id = :id AND status = 'active' FOR NO KEY UPDATE", ['id' => $id]);
            if ($locked === null) {
                throw new ApiError(404, 'relationship_not_found');
            }
        }
    }

    public function isBlocked(string $actor, string $target): bool
    {
        return $this->db->fetchValue(
            'SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id = :actor AND blocked_id = :target) OR (blocker_id = :target AND blocked_id = :actor))',
            ['actor' => $actor, 'target' => $target],
        );
    }

    public function findFriendRequest(string $requestId): ?FriendRequest
    {
        $row = $this->db->fetchOne('SELECT ' . FriendRequest::COLUMNS . ' FROM friend_requests WHERE id = :id', ['id' => $requestId]);
        return $row === null ? null : FriendRequest::fromRow($row);
    }

    public function mustFindFriendRequest(string $requestId): FriendRequest
    {
        return $this->findFriendRequest($requestId) ?? throw new \RuntimeException('sql: no rows in result set');
    }

    public function insertFriendship(FriendRequest $request): void
    {
        [$low, $high] = Ids::ordered($request->requesterId, $request->recipientId);
        $this->db->execute(
            'INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id) VALUES (:id, :low, :high, :request_id)',
            ['id' => Ids::generate(), 'low' => $low, 'high' => $high, 'request_id' => $request->id],
        );
    }
}
