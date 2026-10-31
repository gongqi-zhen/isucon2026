package Isureal::Comment;
use v5.44;
use utf8;

use Exporter 'import';
use Isureal::Post qw(get_post_with_author load_viewer_context can_view);
use Isureal::User qw(extract_user user_columns_as relationship_user_response);
use Isureal::Util qw(rfc3339);

our @EXPORT_OK = qw(get_comment select_comments_with_author can_comment comment_response);

sub get_comment ($db, $id) {
    return $db->query(q{SELECT id,post_id,author_id,body,created_at,deleted_at FROM comments WHERE id=$1}, $id)->hash;
}

sub select_comments_with_author ($db, $conditions, @args) {
    my $comments = $db->query(<<~"SQL", @args)->hashes;
          SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, ${\user_columns_as('u', 'author')}
          FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
          WHERE c.deleted_at IS NULL $conditions
        SQL
    return $comments->map(sub ($row) { +{ comment => $row, author => extract_user($row, 'author') } });
}

sub can_comment ($db, $viewer_id, $post_id) {
    my $post   = get_post_with_author($db, $post_id)  or return false;
    my $viewer = load_viewer_context($db, $viewer_id) or return false;
    $post = $post->{post};
    return false if !can_view($viewer, $post) || defined $post->{window_ended_at};
    return !!$viewer->{has_posted_today} if $viewer->{id} eq $post->{author_id};
    return true if $post->{visibility} eq 'public';
    return !!$viewer->{friends}{$post->{author_id}};
}

sub comment_response ($comment, $user) {
    return {
        id        => $comment->{id},
        author    => relationship_user_response($user),
        body      => $comment->{body},
        createdAt => rfc3339($comment->{created_at}),
    };
}

1;
