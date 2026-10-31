<?php

declare(strict_types=1);

namespace Isureal\Handler;

use Isureal\Database;
use Isureal\Http\ApiError;
use Isureal\Http\Input;
use Isureal\Http\Json;
use Isureal\Http\Middleware\SessionMiddleware;
use Isureal\Model\User;
use Isureal\Repository\PostRepository;
use Isureal\Repository\RelationshipRepository;
use Isureal\Repository\UserRepository;
use Normalizer;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class UserHandler
{
    public function __construct(
        private Database $db,
        private UserRepository $users,
        private PostRepository $posts,
        private RelationshipRepository $relationships,
    ) {
    }

    public static function normalizeSearchText(string $value): string
    {
        $folded = mb_convert_case((string) Normalizer::normalize($value, Normalizer::NFC), MB_CASE_FOLD, 'UTF-8');
        return (string) Normalizer::normalize($folded, Normalizer::NFC);
    }

    /**
     * GET /api/users
     */
    public function search(Request $request, Response $response): Response
    {
        $q = Input::queryString($request, 'q');
        $beforeUsername = Input::queryString($request, 'beforeUsername');
        $beforeId = Input::queryString($request, 'beforeId');
        if (!User::validText($q, 1, 100)) {
            throw new ApiError(400, 'invalid_relationship');
        }
        if (($beforeUsername === '') !== ($beforeId === '')) {
            throw new ApiError(400, 'invalid_pagination');
        }
        $viewer = SessionMiddleware::session($request)->userId;
        $rows = $this->db->fetchAll(
            'SELECT ' . User::columns() . " FROM users
             WHERE status = 'active' AND id <> :viewer AND (:before_username = '' OR (username, id) > (:before_username, :before_id))
             ORDER BY username, id",
            ['viewer' => $viewer, 'before_username' => $beforeUsername, 'before_id' => $beforeId],
        );
        $blocked = $this->posts->blockedUserIds($viewer);
        $query = self::normalizeSearchText($q);
        $items = [];
        foreach ($rows as $row) {
            $user = User::fromRow($row);
            if (!str_contains(self::normalizeSearchText($user->username), $query) && !str_contains(self::normalizeSearchText($user->displayName), $query)) {
                continue;
            }
            if (isset($blocked[$user->id])) {
                continue;
            }
            if (!$user->isCertified()) {
                $user->bio = '';
            }
            $items[] = $user->toPublic();
            if (count($items) === 51) {
                break;
            }
        }
        return Json::respond($response, self::page($items));
    }

    /**
     * GET /api/users/{userId}
     *
     * @param array<string, string> $args
     */
    public function show(Request $request, Response $response, array $args): Response
    {
        $viewer = SessionMiddleware::session($request)->userId;
        $user = $this->users->find($args['userId']) ?? throw new ApiError(404, 'relationship_not_found');
        if ($this->relationships->isBlocked($viewer, $user->id)) {
            throw new ApiError(404, 'relationship_not_found');
        }
        $friends = $this->posts->friendIds($viewer);
        if (!$user->isCertified() && !in_array($user->id, $friends, true)) {
            $user->bio = '';
        }
        return Json::respond($response, $user->toPublic());
    }

    /**
     * 51 件まで集めた一覧を 50 件の 1 ページにする
     *
     * @param list<mixed> $items
     * @return array{items: list<mixed>, hasMore: bool}
     */
    public static function page(array $items): array
    {
        return ['items' => array_slice($items, 0, 50), 'hasMore' => count($items) > 50];
    }
}
