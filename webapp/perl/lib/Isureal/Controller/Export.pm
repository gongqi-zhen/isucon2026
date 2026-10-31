package Isureal::Controller::Export;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use IO::Compress::Zip qw(:zip_method);
use Mojo::JSON qw(encode_json);
use Isureal::Post qw(POST_COLUMNS post_response);
use Isureal::User qw(get_user);
use Isureal::Util qw(NO_ROWS);

sub export_posts ($c) {
    my $user_id = $c->current_session->{user_id};
    my $db      = $c->db;
    my $author  = get_user($db, $user_id) // die NO_ROWS;
    my $posts   = $db->query(<<~"SQL", $user_id)->hashes;
           SELECT ${\POST_COLUMNS}, i.data AS image_data
           FROM posts p
           JOIN posting_windows w ON w.id = p.posting_window_id
           JOIN image_objects i ON i.id = p.image_id
           WHERE p.author_id = \$1 AND p.deleted_at IS NULL
           ORDER BY p.posted_at, p.id
        SQL

    my (@members, @items);
    for my $post (@$posts) {
        my $image_path = sprintf 'images/%06d.jpg', @items + 1;
        push @members, { name => $image_path, method => ZIP_CM_STORE, data => $post->{image_data} };
        push @items, { post_response($post, $author)->%*, imagePath => $image_path };
    }
    push @members, { name => 'posts.json', method => ZIP_CM_DEFLATE, data => encode_json(\@items) . "\n" };
    my $body = build_zip(@members);

    my $headers = $c->res->headers;
    $headers->content_disposition('attachment; filename="isureal-posts.zip"');
    $headers->cache_control('no-store');
    $headers->content_type('application/zip');
    $c->render(data => $body);
}

sub build_zip ($first, @rest) {
    my $body = '';
    my $zip  = IO::Compress::Zip->new(\$body, Name => $first->{name}, Method => $first->{method}, Minimal => 1)
        or die "failed to create zip: $IO::Compress::Zip::ZipError\n";
    $zip->print($first->{data});
    for my $member (@rest) {
        $zip->newStream(Name => $member->{name}, Method => $member->{method});
        $zip->print($member->{data});
    }
    $zip->close;
    return $body;
}

1;
