package Isureal::Controller::Post;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use DBD::Pg qw(PG_BYTEA);
use Isureal::APIError;
use Isureal::JPEG qw(decode_jpeg);
use Isureal::Post qw(valid_visibility valid_location get_post get_post_with_author load_viewer_context can_view post_response);
use Isureal::PostingWindow qw(get_posting_window);
use Isureal::User qw(get_user valid_user_text);
use Isureal::Util qw(NO_ROWS new_uuid moment now_utc);

sub create ($c) {
    my $form   = $c->form_body;
    my $fields = $form->{fields};
    my $image  = $form->{uploads}{image};
    my %input  = (
        caption       => $fields->{caption}       // '',
        visibility    => $fields->{visibility}    // '',
        location_kind => $fields->{location_kind} // '',
        latitude      => $c->form_number($fields->{latitude}),
        longitude     => $c->form_number($fields->{longitude}),
        locality      => $fields->{locality},
    );
    if (!$image || !valid_user_text($input{caption}, 0, 1000) || !valid_visibility($input{visibility})) {
        Isureal::APIError->throw(400, 'invalid_post');
    }
    my $location = {
        kind     => $input{location_kind},
        latitude => $input{latitude}, longitude => $input{longitude}, locality => $input{locality},
    };
    Isureal::APIError->throw(400, 'invalid_post') unless valid_location($location);
    my $image_data = $image->slurp;
    my $decoded = decode_jpeg($image_data) or Isureal::APIError->throw(400, 'invalid_post');
    Isureal::APIError->throw(400, 'invalid_post') if $decoded->{width} > 4096 || $decoded->{height} > 4096;

    my $user_id = $c->current_session->{user_id};
    my $db      = $c->db;
    my $tx      = $db->begin;
    $db->query(q{SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE}, $user_id)->array or die NO_ROWS;
    my $user   = get_user($db, $user_id) // die NO_ROWS;
    my $window = $db->query(
        q{SELECT id, local_date, starts_at FROM posting_windows WHERE region = $1 AND ended_at IS NULL FOR UPDATE},
        $user->{region},
    )->hash;
    $window //= $db->query(
        q{SELECT id, local_date, starts_at FROM posting_windows WHERE region = $1 AND ended_at IS NULL FOR UPDATE},
        $user->{region},
    )->hash;
    Isureal::APIError->throw(409, 'post_slot_unavailable') unless $window;

    my $counts = $db->query(<<~'SQL', $user_id, $window->{local_date})->hash;
           SELECT count(*) FILTER (WHERE deleted_at IS NULL) AS active,
                  count(*) FILTER (WHERE deleted_at IS NOT NULL) AS deleted
           FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
           WHERE p.author_id = $1 AND w.local_date = $2
        SQL
    my $posted_at = now_utc();
    if ($counts->{active} != 0 || $counts->{deleted} >= 2 || $posted_at->is_before(moment($window->{starts_at}))) {
        Isureal::APIError->throw(409, 'post_slot_unavailable');
    }
    my $image_id = new_uuid();
    $db->query(
        q{INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES ($1, $2, $3, 'image/jpeg', $4)},
        $image_id, $user_id, { type => PG_BYTEA, value => $image_data }, $posted_at->to_string,
    );
    my $post_id = new_uuid();
    $db->query(<<~'SQL',
           INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                              location_kind, latitude, longitude, locality, posted_at)
           VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
        SQL
        $post_id, $user_id, $window->{id}, $image_id, $input{caption}, $input{visibility},
        $location->@{qw(kind latitude longitude locality)}, $posted_at->to_string,
    );
    my $post = get_post($db, $post_id) // die NO_ROWS;
    $tx->commit;
    $c->render(status => 201, json => post_response($post, $user));
}

sub my_current ($c) {
    my $db      = $c->db;
    my $user_id = $c->current_session->{user_id};
    my $row     = $db->query(<<~'SQL', $user_id)->array or Isureal::APIError->throw(404, 'post_not_found');
           SELECT p.id FROM posts p
           JOIN posting_windows w ON w.id = p.posting_window_id
           JOIN users u ON u.id = p.author_id
           JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                      AND current.local_date = w.local_date
           WHERE p.author_id = $1 AND p.deleted_at IS NULL
        SQL
    my $post = get_post($db, $row->[0]) or Isureal::APIError->throw(404, 'post_not_found');
    my $user = get_user($db, $user_id) // die NO_ROWS;
    $c->render(json => post_response($post, $user));
}

sub show ($c) {
    my $db     = $c->db;
    my $post   = get_post_with_author($db, $c->param('postId')) or Isureal::APIError->throw(404, 'post_not_found');
    my $viewer = load_viewer_context($db, $c->current_session->{user_id});
    Isureal::APIError->throw(404, 'post_not_found') unless $viewer && can_view($viewer, $post->{post});
    $c->render(json => post_response($post->{post}, $post->{author}));
}

sub update ($c) {
    my $input = $c->json_body(caption => '?string', visibility => '?string');
    my ($caption, $visibility) = $input->@{qw(caption visibility)};
    if (   (!defined $caption && !defined $visibility)
        || (defined $caption    && !valid_user_text($caption, 0, 1000))
        || (defined $visibility && !valid_visibility($visibility)))
    {
        Isureal::APIError->throw(400, 'invalid_post');
    }
    my $user_id = $c->current_session->{user_id};
    my $db      = $c->db;
    my $tx      = $db->begin;
    $db->query(q{SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE}, $user_id)->array or die NO_ROWS;
    my $post = get_post($db, $c->param('postId')) or Isureal::APIError->throw(404, 'post_not_found');
    Isureal::APIError->throw(403, 'post_not_editable') if $post->{author_id} ne $user_id;
    my $window = get_posting_window($db, $post->{posting_window_id}) // die NO_ROWS;
    Isureal::APIError->throw(403, 'post_not_editable') if defined $window->{ended_at};

    $db->query(q{UPDATE posts SET caption = $2 WHERE id = $1}, $post->{id}, $caption)       if defined $caption;
    $db->query(q{UPDATE posts SET visibility = $2 WHERE id = $1}, $post->{id}, $visibility) if defined $visibility;
    $post = get_post($db, $post->{id}) // die NO_ROWS;
    my $user = get_user($db, $user_id) // die NO_ROWS;
    $tx->commit;
    $c->render(json => post_response($post, $user));
}

sub remove ($c) {
    my $user_id = $c->current_session->{user_id};
    my $db      = $c->db;
    my $tx      = $db->begin;
    $db->query(q{SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE}, $user_id)->array or die NO_ROWS;
    my $post = get_post($db, $c->param('postId')) or Isureal::APIError->throw(404, 'post_not_found');
    Isureal::APIError->throw(403, 'post_not_editable') if $post->{author_id} ne $user_id;
    my $window    = get_posting_window($db, $post->{posting_window_id}) // die NO_ROWS;
    my $deletions = $db->query(<<~'SQL', $user_id, $window->{local_date})->array->[0];
           SELECT count(*) FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
           WHERE p.author_id = $1 AND w.local_date = $2 AND p.deleted_at IS NOT NULL
        SQL
    Isureal::APIError->throw(409, 'post_slot_unavailable') if $deletions >= 2;
    $db->query(q{UPDATE posts SET deleted_at = clock_timestamp() WHERE id = $1}, $post->{id});
    $tx->commit;
    $c->no_content;
}

1;
