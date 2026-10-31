<?php

declare(strict_types=1);

namespace Isureal\Repository;

use Isureal\Database;
use Isureal\Model\Post;
use Isureal\Model\User;
use Isureal\Model\ViewerContext;

final readonly class PostRepository
{
    public function __construct(private Database $db, private UserRepository $users)
    {
    }

    /**
     * 削除されていない投稿を返す
     */
    public function find(string $postId): ?Post
    {
        $row = $this->db->fetchOne(
            'SELECT ' . Post::COLUMNS . '
             FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
             WHERE p.id = :id AND p.deleted_at IS NULL',
            ['id' => $postId],
        );
        return $row === null ? null : Post::fromRow($row);
    }

    public function mustFind(string $postId): Post
    {
        return $this->find($postId) ?? throw new \RuntimeException('sql: no rows in result set');
    }

    /**
     * 投稿を投稿者と一緒に返す。$conditions は WHERE 句に続ける条件
     *
     * @param array<string, mixed> $params
     * @return list<array{Post, User}>
     */
    public function selectWithAuthor(string $conditions, array $params): array
    {
        $rows = $this->db->fetchAll(
            'SELECT ' . Post::COLUMNS . ', ' . User::columnsAs('u', 'author') . "
             FROM posts p
             JOIN posting_windows w ON w.id = p.posting_window_id
             JOIN users u ON u.id = p.author_id AND u.status = 'active'
             WHERE p.deleted_at IS NULL {$conditions}",
            $params,
        );
        return array_map(static fn (array $row): array => [Post::fromRow($row), User::fromRow($row, 'author.')], $rows);
    }

    /**
     * @return array{Post, User}|null
     */
    public function findWithAuthor(string $postId): ?array
    {
        return $this->selectWithAuthor('AND p.id = :post_id', ['post_id' => $postId])[0] ?? null;
    }

    public function loadViewer(string $viewerId): ?ViewerContext
    {
        $user = $this->users->find($viewerId);
        if ($user === null) {
            return null;
        }
        $hasPostedToday = $this->db->fetchValue(
            'SELECT EXISTS(
               SELECT 1 FROM posts p
               JOIN posting_windows posted ON posted.id = p.posting_window_id
               JOIN posting_windows current ON current.region = :region AND current.ended_at IS NULL
                                           AND current.local_date = posted.local_date
               WHERE p.author_id = :user_id AND p.deleted_at IS NULL
             )',
            ['user_id' => $user->id, 'region' => $user->region],
        );
        $blocked = $this->blockedUserIds($user->id);
        $friendIds = $this->friendIds($user->id);
        if ($friendIds === []) {
            return new ViewerContext($user, $hasPostedToday, [], [], [], $blocked);
        }
        $friends = array_fill_keys($friendIds, true);
        [$in, $params] = Database::inClause('friend', $friendIds);
        $friendships = $this->db->fetchAll(
            "SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN ({$in}) OR user_high_id IN ({$in})",
            $params,
        );
        $friendsOfFriends = [];
        $mutualFriends = [];
        foreach ($friendships as ['user_low_id' => $low, 'user_high_id' => $high]) {
            if (isset($friends[$low])) {
                $friendsOfFriends[$high] = true;
                if ($high !== $user->id) {
                    $mutualFriends[$high] = ($mutualFriends[$high] ?? 0) + 1;
                }
            }
            if (isset($friends[$high])) {
                $friendsOfFriends[$low] = true;
                if ($low !== $user->id) {
                    $mutualFriends[$low] = ($mutualFriends[$low] ?? 0) + 1;
                }
            }
        }
        return new ViewerContext($user, $hasPostedToday, $friends, $friendsOfFriends, $mutualFriends, $blocked);
    }

    public function canView(string $viewerId, string $postId): bool
    {
        $found = $this->findWithAuthor($postId);
        if ($found === null) {
            return false;
        }
        return $this->loadViewer($viewerId)?->canView($found[0]) ?? false;
    }

    /**
     * @return list<string>
     */
    public function friendIds(string $userId): array
    {
        $friendships = $this->db->fetchAll(
            'SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id = :user_id OR user_high_id = :user_id',
            ['user_id' => $userId],
        );
        return array_map(
            static fn (array $friendship): string => $friendship['user_low_id'] === $userId ? $friendship['user_high_id'] : $friendship['user_low_id'],
            $friendships,
        );
    }

    /**
     * ブロックしている、またはブロックされているユーザー
     *
     * @return array<string, true>
     */
    public function blockedUserIds(string $userId): array
    {
        $ids = $this->db->fetchColumn(
            'SELECT CASE WHEN blocker_id = :user_id THEN blocked_id ELSE blocker_id END
             FROM block_relations WHERE blocker_id = :user_id OR blocked_id = :user_id',
            ['user_id' => $userId],
        );
        return array_fill_keys($ids, true);
    }
}
