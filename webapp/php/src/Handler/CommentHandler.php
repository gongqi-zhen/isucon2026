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
use Isureal\Repository\CommentRepository;
use Isureal\Repository\PostRepository;
use Isureal\Repository\UserRepository;
use Isureal\Time;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class CommentHandler
{
    public function __construct(
        private Database $db,
        private UserRepository $users,
        private PostRepository $posts,
        private CommentRepository $comments,
    ) {
    }

    /**
     * POST /api/posts/{postId}/comments
     *
     * @param array<string, string> $args
     */
    public function create(Request $request, Response $response, array $args): Response
    {
        $body = Input::json($request)->string('body');
        if (!User::validText($body, 1, 1000)) {
            throw new ApiError(400, 'invalid_interaction');
        }
        $actor = SessionMiddleware::session($request)->userId;
        $post = $this->posts->find($args['postId']) ?? throw new ApiError(403, 'interaction_forbidden');
        [$comment, $user] = $this->db->transaction(function (Database $db) use ($actor, $post, $body): array {
            foreach (array_unique(Ids::ordered($actor, $post->authorId)) as $id) {
                $this->users->mustLock($id);
            }
            if (!$this->comments->canComment($actor, $post->id)) {
                throw new ApiError(403, 'interaction_forbidden');
            }
            $id = Ids::generate();
            $db->execute(
                'INSERT INTO comments (id, post_id, author_id, body, created_at) VALUES (:id, :post_id, :author_id, :body, clock_timestamp())',
                ['id' => $id, 'post_id' => $post->id, 'author_id' => $actor, 'body' => $body],
            );
            return [$this->comments->mustFind($id), $this->users->mustFind($actor)];
        });
        return Json::respond($response, $comment->toResponse($user), 201);
    }

    /**
     * GET /api/posts/{postId}/comments
     *
     * @param array<string, string> $args
     */
    public function list(Request $request, Response $response, array $args): Response
    {
        $afterCreatedAt = Input::queryTime($request, 'afterCreatedAt');
        $afterId = Input::queryString($request, 'afterId');
        if (($afterCreatedAt === null) !== ($afterId === '')) {
            throw new ApiError(400, 'invalid_pagination');
        }
        if (!$this->comments->canComment(SessionMiddleware::session($request)->userId, $args['postId'])) {
            throw new ApiError(403, 'interaction_forbidden');
        }
        $comments = $this->comments->selectWithAuthor(
            'AND c.post_id = :post_id AND (:after_created_at::timestamptz IS NULL OR (c.created_at, c.id) > (:after_created_at, :after_id))
             ORDER BY c.created_at, c.id LIMIT 51',
            [
                'post_id' => $args['postId'],
                'after_created_at' => $afterCreatedAt === null ? null : Time::toDb($afterCreatedAt),
                'after_id' => $afterId,
            ],
        );
        $items = array_map(static fn (array $found): array => $found[0]->toResponse($found[1]), $comments);
        return Json::respond($response, UserHandler::page($items));
    }

    /**
     * DELETE /api/comments/{commentId}
     *
     * @param array<string, string> $args
     */
    public function delete(Request $request, Response $response, array $args): Response
    {
        $actor = SessionMiddleware::session($request)->userId;
        $this->db->transaction(function (Database $db) use ($actor, $args): void {
            $this->users->mustLock($actor);
            $record = $db->fetchOne(
                'SELECT c.author_id, p.author_id AS post_author_id FROM comments c JOIN posts p ON p.id = c.post_id
                 WHERE c.id = :id AND c.deleted_at IS NULL FOR UPDATE OF c',
                ['id' => $args['commentId']],
            ) ?? throw new ApiError(404, 'interaction_not_found');
            if ($actor !== $record['author_id'] && $actor !== $record['post_author_id']) {
                throw new ApiError(403, 'interaction_forbidden');
            }
            $db->execute('UPDATE comments SET deleted_at = clock_timestamp() WHERE id = :id', ['id' => $args['commentId']]);
        });
        return Json::noContent($response);
    }
}
