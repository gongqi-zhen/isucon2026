<?php

declare(strict_types=1);

namespace Isureal\Handler;

use Isureal\Bytea;
use Isureal\Database;
use Isureal\Http\ApiError;
use Isureal\Http\Input;
use Isureal\Http\Json;
use Isureal\Http\Middleware\SessionMiddleware;
use Isureal\Ids;
use Isureal\Jpeg;
use Isureal\Model\RealMoji;
use Isureal\Repository\PostRepository;
use Isureal\Repository\RealMojiRepository;
use Isureal\Repository\RelationshipRepository;
use Isureal\Repository\UserRepository;
use Isureal\Time;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class RealMojiHandler
{
    public function __construct(
        private Database $db,
        private UserRepository $users,
        private PostRepository $posts,
        private RealMojiRepository $realMojis,
        private RelationshipRepository $relationships,
    ) {
    }

    /**
     * POST /api/posts/{postId}/realmojis
     *
     * @param array<string, string> $args
     */
    public function replace(Request $request, Response $response, array $args): Response
    {
        [$fields, $files] = Input::form($request);
        $image = $files['image'] ?? throw new ApiError(400, 'invalid_realmoji');
        $reactionType = $fields['reactionType'] ?? '';
        if (!in_array($reactionType, RealMoji::REACTION_TYPES, true)) {
            throw new ApiError(400, 'invalid_realmoji');
        }
        $data = (string) $image->getStream();
        [$width, $height] = Jpeg::size($data) ?? throw new ApiError(400, 'invalid_realmoji');
        if ($width > 512 || $height > 512) {
            throw new ApiError(400, 'invalid_realmoji');
        }

        $actor = SessionMiddleware::session($request)->userId;
        $post = $this->posts->find($args['postId']) ?? throw new ApiError(403, 'interaction_forbidden');
        if ($actor === $post->authorId) {
            throw new ApiError(403, 'interaction_forbidden');
        }
        [$reaction, $user] = $this->db->transaction(function (Database $db) use ($actor, $post, $data, $reactionType): array {
            $this->relationships->lockPair($actor, $post->authorId);
            if (!$this->posts->canView($actor, $post->id)) {
                throw new ApiError(403, 'interaction_forbidden');
            }
            $db->execute(
                'UPDATE realmojis SET replaced_at = clock_timestamp()
                 WHERE post_id = :post_id AND reactor_id = :reactor_id AND replaced_at IS NULL AND deleted_at IS NULL',
                ['post_id' => $post->id, 'reactor_id' => $actor],
            );
            $imageId = Ids::generate();
            $db->execute(
                "INSERT INTO image_objects (id, owner_user_id, data, media_type) VALUES (:id, :owner_user_id, :data, 'image/jpeg')",
                ['id' => $imageId, 'owner_user_id' => $actor, 'data' => new Bytea($data)],
            );
            $id = Ids::generate();
            $db->execute(
                'INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at)
                 VALUES (:id, :post_id, :reactor_id, :image_id, :reaction_type, clock_timestamp())',
                ['id' => $id, 'post_id' => $post->id, 'reactor_id' => $actor, 'image_id' => $imageId, 'reaction_type' => $reactionType],
            );
            return [$this->realMojis->mustFind($id), $this->users->mustFind($actor)];
        });
        return Json::respond($response, $reaction->toResponse($user), 201);
    }

    /**
     * GET /api/posts/{postId}/realmojis
     *
     * @param array<string, string> $args
     */
    public function list(Request $request, Response $response, array $args): Response
    {
        $beforeCreatedAt = Input::queryTime($request, 'beforeCreatedAt');
        $beforeId = Input::queryString($request, 'beforeId');
        if (($beforeCreatedAt === null) !== ($beforeId === '')) {
            throw new ApiError(400, 'invalid_pagination');
        }
        if (!$this->posts->canView(SessionMiddleware::session($request)->userId, $args['postId'])) {
            throw new ApiError(403, 'interaction_forbidden');
        }
        $reactions = $this->realMojis->selectCurrent(
            'AND r.post_id = :post_id AND (:before_created_at::timestamptz IS NULL OR (r.created_at, r.id) < (:before_created_at, :before_id))
             ORDER BY r.created_at DESC, r.id DESC LIMIT 51',
            [
                'post_id' => $args['postId'],
                'before_created_at' => $beforeCreatedAt === null ? null : Time::toDb($beforeCreatedAt),
                'before_id' => $beforeId,
            ],
        );
        $items = array_map(static fn (array $found): array => $found[0]->toResponse($found[1]), $reactions);
        return Json::respond($response, UserHandler::page($items));
    }

    /**
     * DELETE /api/realmojis/{realMojiId}
     *
     * @param array<string, string> $args
     */
    public function delete(Request $request, Response $response, array $args): Response
    {
        $actor = SessionMiddleware::session($request)->userId;
        $this->db->transaction(function (Database $db) use ($actor, $args): void {
            $this->users->mustLock($actor);
            $record = $db->fetchOne(
                'SELECT r.reactor_id, p.author_id FROM realmojis r JOIN posts p ON p.id = r.post_id
                 WHERE r.id = :id AND r.deleted_at IS NULL FOR UPDATE OF r',
                ['id' => $args['realMojiId']],
            ) ?? throw new ApiError(404, 'interaction_not_found');
            if ($actor !== $record['reactor_id'] && $actor !== $record['author_id']) {
                throw new ApiError(403, 'interaction_forbidden');
            }
            $db->execute('UPDATE realmojis SET deleted_at = clock_timestamp() WHERE id = :id', ['id' => $args['realMojiId']]);
        });
        return Json::noContent($response);
    }
}
