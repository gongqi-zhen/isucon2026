<?php

declare(strict_types=1);

namespace Isureal\Handler;

use Isureal\Bytea;
use Isureal\Database;
use Isureal\Http\Json;
use Isureal\Http\Middleware\SessionMiddleware;
use Isureal\Model\Post;
use Isureal\Repository\UserRepository;
use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;
use ZipArchive;

final readonly class ExportHandler
{
    public function __construct(private Database $db, private UserRepository $users)
    {
    }

    /**
     * GET /api/me/export
     */
    public function export(Request $request, Response $response): Response
    {
        $userId = SessionMiddleware::session($request)->userId;
        $author = $this->users->mustFind($userId);
        $rows = $this->db->fetchAll(
            'SELECT ' . Post::COLUMNS . ', i.data AS image_data
             FROM posts p
             JOIN posting_windows w ON w.id = p.posting_window_id
             JOIN image_objects i ON i.id = p.image_id
             WHERE p.author_id = :author_id AND p.deleted_at IS NULL
             ORDER BY p.posted_at, p.id',
            ['author_id' => $userId],
        );

        $path = (string) tempnam(sys_get_temp_dir(), 'isureal-export-');
        try {
            $zip = new ZipArchive();
            if ($zip->open($path, ZipArchive::CREATE | ZipArchive::OVERWRITE) !== true) {
                throw new \RuntimeException('failed to create zip');
            }
            $items = [];
            foreach ($rows as $row) {
                $imagePath = sprintf('images/%06d.jpg', count($items) + 1);
                $zip->addFromString($imagePath, Bytea::read($row['image_data']));
                $zip->setCompressionName($imagePath, ZipArchive::CM_STORE);
                $items[] = [...Post::fromRow($row)->toResponse($author), 'imagePath' => $imagePath];
            }
            $zip->addFromString('posts.json', Json::encode($items) . "\n");
            if (!$zip->close()) {
                throw new \RuntimeException('failed to write zip');
            }
            $response->getBody()->write((string) file_get_contents($path));
        } finally {
            @unlink($path);
        }
        return $response
            ->withHeader('Content-Disposition', 'attachment; filename="isureal-posts.zip"')
            ->withHeader('Cache-Control', 'no-store')
            ->withHeader('Content-Type', 'application/zip');
    }
}
