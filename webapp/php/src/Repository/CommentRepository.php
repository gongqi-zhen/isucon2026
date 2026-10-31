<?php

declare(strict_types=1);

namespace Isureal\Repository;

use Isureal\Database;
use Isureal\Model\Comment;
use Isureal\Model\User;
use Isureal\Model\Visibility;

final readonly class CommentRepository
{
    public function __construct(private Database $db, private PostRepository $posts)
    {
    }

    public function find(string $commentId): ?Comment
    {
        $row = $this->db->fetchOne(
            'SELECT id, post_id, author_id, body, created_at, deleted_at FROM comments WHERE id = :id',
            ['id' => $commentId],
        );
        return $row === null ? null : Comment::fromRow($row);
    }

    public function mustFind(string $commentId): Comment
    {
        return $this->find($commentId) ?? throw new \RuntimeException('sql: no rows in result set');
    }

    /**
     * 削除されていないコメントを投稿者と一緒に返す。$conditions は WHERE 句に続ける条件
     *
     * @param array<string, mixed> $params
     * @return list<array{Comment, User}>
     */
    public function selectWithAuthor(string $conditions, array $params): array
    {
        $rows = $this->db->fetchAll(
            'SELECT ' . Comment::COLUMNS . ', ' . User::columnsAs('u', 'author') . "
             FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
             WHERE c.deleted_at IS NULL {$conditions}",
            $params,
        );
        return array_map(static fn (array $row): array => [Comment::fromRow($row), User::fromRow($row, 'author.')], $rows);
    }

    public function canComment(string $viewerId, string $postId): bool
    {
        $found = $this->posts->findWithAuthor($postId);
        if ($found === null) {
            return false;
        }
        [$post] = $found;
        $viewer = $this->posts->loadViewer($viewerId);
        if ($viewer === null || !$viewer->canView($post) || $post->windowEndedAt !== null) {
            return false;
        }
        if ($viewer->user->id === $post->authorId) {
            return $viewer->hasPostedToday;
        }
        if ($post->visibility === Visibility::Public->value) {
            return true;
        }
        return $viewer->isFriend($post->authorId);
    }
}
