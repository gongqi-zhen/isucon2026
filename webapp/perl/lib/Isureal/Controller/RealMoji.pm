package Isureal::Controller::RealMoji;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use DBD::Pg qw(PG_BYTEA);
use Isureal::APIError;
use Isureal::JPEG qw(decode_jpeg);
use Isureal::Post qw(get_post can_view_post);
use Isureal::RealMoji qw(get_real_moji select_current_real_mojis real_moji_response);
use Isureal::Relationship qw(begin_relationship_tx);
use Isureal::User qw(get_user);
use Isureal::Util qw(NO_ROWS new_uuid);

sub replace ($c) {
    my $form          = $c->form_body;
    my $image         = $form->{uploads}{image} or Isureal::APIError->throw(400, 'invalid_realmoji');
    my $reaction_type = $form->{fields}{reactionType} // '';
    unless ($reaction_type =~ /\A(?:like|smile|surprise|heart|laugh|instant)\z/) {
        Isureal::APIError->throw(400, 'invalid_realmoji');
    }
    my $data    = $image->slurp;
    my $decoded = decode_jpeg($data) or Isureal::APIError->throw(400, 'invalid_realmoji');
    Isureal::APIError->throw(400, 'invalid_realmoji') if $decoded->{width} > 512 || $decoded->{height} > 512;

    my $db    = $c->db;
    my $actor = $c->current_session->{user_id};
    my $post  = get_post($db, $c->param('postId')) or Isureal::APIError->throw(403, 'interaction_forbidden');
    Isureal::APIError->throw(403, 'interaction_forbidden') if $actor eq $post->{author_id};
    my $tx = begin_relationship_tx($db, $actor, $post->{author_id});
    Isureal::APIError->throw(403, 'interaction_forbidden') unless can_view_post($db, $actor, $post->{id});

    $db->query(
        q{UPDATE realmojis SET replaced_at=clock_timestamp() WHERE post_id=$1 AND reactor_id=$2 AND replaced_at IS NULL AND deleted_at IS NULL},
        $post->{id}, $actor,
    );
    my $image_id = new_uuid();
    $db->query(
        q{INSERT INTO image_objects(id,owner_user_id,data,media_type) VALUES($1,$2,$3,'image/jpeg')},
        $image_id, $actor, { type => PG_BYTEA, value => $data },
    );
    my $id = new_uuid();
    $db->query(
        q{INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at) VALUES($1,$2,$3,$4,$5,clock_timestamp())},
        $id, $post->{id}, $actor, $image_id, $reaction_type,
    );
    my $reaction = get_real_moji($db, $id) // die NO_ROWS;
    my $user     = get_user($db, $actor)   // die NO_ROWS;
    $tx->commit;
    $c->render(status => 201, json => real_moji_response($reaction, $user));
}

sub list ($c) {
    my $before_created_at = $c->query_time('beforeCreatedAt');
    my $before_id         = $c->query_string('beforeId');
    Isureal::APIError->throw(400, 'invalid_pagination') if defined $before_created_at xor $before_id ne '';

    my $db      = $c->db;
    my $post_id = $c->param('postId');
    Isureal::APIError->throw(403, 'interaction_forbidden') unless can_view_post($db, $c->current_session->{user_id}, $post_id);
    my $reactions = select_current_real_mojis(
        $db,
        q{AND r.post_id=$1 AND ($2::timestamptz IS NULL OR (r.created_at,r.id)<($2,$3)) ORDER BY r.created_at DESC,r.id DESC LIMIT 51},
        $post_id, $before_created_at && $before_created_at->to_string, $before_id,
    );
    my @items    = map { real_moji_response($_->{real_moji}, $_->{reactor}) } @$reactions;
    my $has_more = @items > 50;
    splice @items, 50 if $has_more;
    $c->render(json => { items => \@items, hasMore => $has_more });
}

sub remove ($c) {
    my $real_moji_id = $c->param('realMojiId');
    my $actor        = $c->current_session->{user_id};
    my $db           = $c->db;
    my $tx           = $db->begin;
    $db->query(q{SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE}, $actor)->array or die NO_ROWS;
    my $record = $db->query(
        q{SELECT r.reactor_id,p.author_id FROM realmojis r JOIN posts p ON p.id=r.post_id WHERE r.id=$1 AND r.deleted_at IS NULL FOR UPDATE OF r},
        $real_moji_id,
    )->hash or Isureal::APIError->throw(404, 'interaction_not_found');
    if ($actor ne $record->{reactor_id} && $actor ne $record->{author_id}) {
        Isureal::APIError->throw(403, 'interaction_forbidden');
    }
    $db->query(q{UPDATE realmojis SET deleted_at=clock_timestamp() WHERE id=$1}, $real_moji_id);
    $tx->commit;
    $c->no_content;
}

1;
