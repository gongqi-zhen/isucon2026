<?php

declare(strict_types=1);

namespace Isureal\Handler;

use Isureal\Database;
use Isureal\Http\ApiError;
use Isureal\Http\Input;
use Isureal\Http\Json;
use Isureal\Http\Middleware\AdminSessionMiddleware;
use Isureal\Ids;
use Isureal\Model\PostingWindow;
use Isureal\Model\Region;
use Isureal\Model\User;
use Isureal\Repository\AdminSessionRepository;
use Isureal\Repository\PostingWindowRepository;
use Isureal\Time;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;

final readonly class AdminHandler
{
    public function __construct(
        private Database $db,
        private AdminSessionRepository $sessions,
        private PostingWindowRepository $windows,
    ) {
    }

    /**
     * POST /api/admin/auth/login
     */
    public function login(Request $request, Response $response): Response
    {
        $input = Input::json($request);
        [$username, $password] = [$input->string('username'), $input->string('password')];
        if (!User::validUsername($username)) {
            throw new ApiError(401, 'invalid_credentials');
        }
        $administrator = $this->db->fetchOne('SELECT id, password_hash FROM administrators WHERE username = :username', ['username' => $username]);
        if ($administrator === null || $administrator['password_hash'] !== Ids::sha256($password)) {
            throw new ApiError(401, 'invalid_credentials');
        }
        $session = $this->sessions->create($administrator['id']);
        return Json::respond($response, ['token' => $session->token, 'administratorId' => $session->administratorId]);
    }

    /**
     * POST /api/admin/auth/logout
     */
    public function logout(Request $request, Response $response): Response
    {
        $session = AdminSessionMiddleware::session($request);
        $this->sessions->invalidate($session->administratorId, $session->tokenHash);
        return Json::noContent($response);
    }

    /**
     * PUT /api/admin/users/{userId}/certification
     *
     * @param array<string, string> $args
     */
    public function certifyUser(Request $request, Response $response, array $args): Response
    {
        $updated = $this->db->execute(
            "UPDATE users SET account_type = 'certified_public', updated_at = clock_timestamp()
             WHERE id = :id AND status = 'active' AND account_type = 'regular'",
            ['id' => $args['userId']],
        );
        if ($updated === 0) {
            throw new ApiError(409, 'certification_conflict');
        }
        return Json::noContent($response);
    }

    /**
     * DELETE /api/admin/users/{userId}/certification
     *
     * 2026-08-26: 認定されたユーザーを戻すことは禁止する
     */
    public function deleteUserCertification(Request $request, Response $response): Response
    {
        throw new ApiError(409, 'certification_irreversible');
    }

    /**
     * POST /api/admin/posting-window
     */
    public function openPostingWindow(Request $request, Response $response): Response
    {
        $input = Input::json($request);
        $region = $input->string('region');
        $localDate = Time::parseDate($input->string('localDate'));
        $onTimeSeconds = $input->optionalInt('onTimeSeconds');
        if (!Region::isValid($region) || $localDate === null) {
            throw new ApiError(400, 'invalid_posting_window');
        }
        $seconds = $onTimeSeconds ?? 24 * 60 * 60;
        $window = $this->db->transaction(function (Database $db) use ($region, $localDate, $seconds): PostingWindow {
            $db->execute('LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE');
            $conflict = $db->fetchValue(
                'SELECT EXISTS(SELECT 1 FROM posting_windows WHERE region = :region AND local_date >= :local_date)',
                ['region' => $region, 'local_date' => $localDate->format('Y-m-d')],
            );
            if ($conflict) {
                throw new ApiError(409, 'posting_window_conflict');
            }
            $startsAt = Time::fromDb($db->fetchValue('SELECT clock_timestamp()'));
            $db->execute(
                'UPDATE posting_windows SET ended_at = :ended_at WHERE region = :region AND ended_at IS NULL',
                ['region' => $region, 'ended_at' => Time::toDb($startsAt)],
            );
            $windowId = Ids::generate();
            $db->execute(
                'INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
                 VALUES (:id, :region, :local_date, :starts_at, :on_time_deadline)',
                [
                    'id' => $windowId,
                    'region' => $region,
                    'local_date' => $localDate->format('Y-m-d'),
                    'starts_at' => Time::toDb($startsAt),
                    'on_time_deadline' => Time::toDb($startsAt->modify("{$seconds} seconds")),
                ],
            );
            $db->execute(
                "INSERT INTO notifications (id, user_id, posting_window_id, created_at)
                 SELECT gen_random_uuid()::text, id, :window_id, :created_at FROM users WHERE region = :region AND status = 'active'",
                ['region' => $region, 'window_id' => $windowId, 'created_at' => Time::toDb($startsAt)],
            );
            return $this->windows->mustFind($windowId);
        });
        return Json::respond($response, $window->toResponse(), 201);
    }
}
