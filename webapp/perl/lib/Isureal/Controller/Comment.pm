package Isureal::Controller::Comment;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use List::Util qw(uniq);
use Isureal::APIError;
use Isureal::Comment qw(get_comment select_comments_with_author can_comment comment_response);
use Isureal::Post qw(get_post);
use Isureal::User qw(get_user valid_user_text);
use Isureal::Util qw(NO_ROWS new_uuid min_max);

sub create ($c) {
    my $input = $c->json_body(body => 'string');
    Isureal::APIError->throw(400, 'invalid_interaction') unless valid_user_text($input->{body}, 1, 1000);

    my $db    = $c->db;
    my $actor = $c->current_session->{user_id};
    my $post  = get_post($db, $c->param('postId')) or Isureal::APIError->throw(403, 'interaction_forbidden');
    my $tx    = $db->begin;
    for my $id (uniq min_max($actor, $post->{author_id})) {
        $db->query(q{SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE}, $id)->array or die NO_ROWS;
    }
    Isureal::APIError->throw(403, 'interaction_forbidden') unless can_comment($db, $actor, $post->{id});
    my $id = new_uuid();
    $db->query(
        q{INSERT INTO comments(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,clock_timestamp())},
        $id, $post->{id}, $actor, $input->{body},
    );
    my $comment = get_comment($db, $id)    // die NO_ROWS;
    my $user    = get_user($db, $actor)    // die NO_ROWS;
    $tx->commit;
    $c->render(status => 201, json => comment_response($comment, $user));
}

sub list ($c) {
    my $after_created_at = $c->query_time('afterCreatedAt');
    my $after_id         = $c->query_string('afterId');
    Isureal::APIError->throw(400, 'invalid_pagination') if defined $after_created_at xor $after_id ne '';

    my $db      = $c->db;
    my $post_id = $c->param('postId');
    Isureal::APIError->throw(403, 'interaction_forbidden') unless can_comment($db, $c->current_session->{user_id}, $post_id);
    my $comments = select_comments_with_author(
        $db,
        q{AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3)) ORDER BY c.created_at,c.id LIMIT 51},
        $post_id, $after_created_at && $after_created_at->to_string, $after_id,
    );
    my @items    = map { comment_response($_->{comment}, $_->{author}) } @$comments;
    my $has_more = @items > 50;
    splice @items, 50 if $has_more;
    $c->render(json => { items => \@items, hasMore => $has_more });
}

sub remove ($c) {
    my $comment_id = $c->param('commentId');
    my $actor      = $c->current_session->{user_id};
    my $db         = $c->db;
    my $tx         = $db->begin;
    $db->query(q{SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE}, $actor)->array or die NO_ROWS;
    my $record = $db->query(
        q{SELECT c.author_id,p.author_id AS post_author_id FROM comments c JOIN posts p ON p.id=c.post_id WHERE c.id=$1 AND c.deleted_at IS NULL FOR UPDATE OF c},
        $comment_id,
    )->hash or Isureal::APIError->throw(404, 'interaction_not_found');
    if ($actor ne $record->{author_id} && $actor ne $record->{post_author_id}) {
        Isureal::APIError->throw(403, 'interaction_forbidden');
    }
    $db->query(q{UPDATE comments SET deleted_at=clock_timestamp() WHERE id=$1}, $comment_id);
    $tx->commit;
    $c->no_content;
}

1;
