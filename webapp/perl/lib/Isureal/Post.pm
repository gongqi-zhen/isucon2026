package Isureal::Post;
use v5.44;
use utf8;

use Exporter 'import';
use Isureal::ImageSignature qw(signed_image_url);
use Isureal::User qw(get_user extract_user user_columns_as relationship_user_response valid_user_text);
use Isureal::Util qw(moment placeholders rfc3339);

our @EXPORT_OK = qw(
    POST_COLUMNS
    valid_visibility
    valid_location
    get_post
    select_posts_with_author
    get_post_with_author
    load_viewer_context
    can_view
    can_view_post
    get_friend_ids
    get_blocked_user_ids
    post_response
);

use constant POST_COLUMNS => <<~'SQL';
    p.id, p.author_id, p.posting_window_id, p.image_id, p.caption, p.visibility,
      p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at,
      w.on_time_deadline, w.ended_at AS window_ended_at
    SQL

sub valid_visibility ($visibility) {
    return defined $visibility && $visibility =~ /\A(?:friends|friends_of_friends|public)\z/;
}

sub valid_location ($location) {
    my ($kind, $latitude, $longitude, $locality) = $location->@{qw(kind latitude longitude locality)};
    return false unless defined $kind;
    if ($kind eq 'none') {
        return !defined $latitude && !defined $longitude && !defined $locality;
    }
    if ($kind eq 'exact') {
        return defined $latitude && defined $longitude && !defined $locality
            && $latitude >= -90 && $latitude <= 90 && $longitude >= -180 && $longitude <= 180;
    }
    if ($kind eq 'approximate') {
        return !defined $latitude && !defined $longitude && defined $locality && valid_user_text($locality, 1, 100);
    }
    return false;
}

sub get_post ($db, $post_id) {
    return $db->query(<<~"SQL", $post_id)->hash;
          SELECT ${\POST_COLUMNS}
          FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
          WHERE p.id = \$1 AND p.deleted_at IS NULL
        SQL
}

sub select_posts_with_author ($db, $conditions, @args) {
    my $posts = $db->query(<<~"SQL", @args)->hashes;
          SELECT ${\POST_COLUMNS}, ${\user_columns_as('u', 'author')}
          FROM posts p
          JOIN posting_windows w ON w.id = p.posting_window_id
          JOIN users u ON u.id = p.author_id AND u.status = 'active'
          WHERE p.deleted_at IS NULL $conditions
        SQL
    return $posts->map(sub ($row) { +{ post => $row, author => extract_user($row, 'author') } });
}

sub get_post_with_author ($db, $post_id) {
    return select_posts_with_author($db, q{AND p.id = $1}, $post_id)->first;
}

sub load_viewer_context ($db, $viewer_id) {
    my $user = get_user($db, $viewer_id) or return undef;
    my $viewer = { %$user, friends => {}, friends_of_friends => {}, mutual_friends => {} };
    $viewer->{has_posted_today} = $db->query(<<~'SQL', $viewer->{id}, $viewer->{region})->array->[0];
          SELECT EXISTS(
            SELECT 1 FROM posts p
            JOIN posting_windows posted ON posted.id = p.posting_window_id
            JOIN posting_windows current ON current.region = $2 AND current.ended_at IS NULL
                                        AND current.local_date = posted.local_date
            WHERE p.author_id = $1 AND p.deleted_at IS NULL
          )
        SQL
    $viewer->{blocked} = get_blocked_user_ids($db, $viewer->{id});

    my @friends = get_friend_ids($db, $viewer->{id});
    return $viewer unless @friends;
    $viewer->{friends}{$_} = true for @friends;

    my $in = placeholders(scalar @friends);
    my $friendships = $db->query(
        "SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN ($in) OR user_high_id IN ($in)",
        @friends,
    )->hashes;
    for my $friendship (@$friendships) {
        my ($low, $high) = $friendship->@{qw(user_low_id user_high_id)};
        if ($viewer->{friends}{$low}) {
            $viewer->{friends_of_friends}{$high} = true;
            $viewer->{mutual_friends}{$high}++ if $high ne $viewer->{id};
        }
        if ($viewer->{friends}{$high}) {
            $viewer->{friends_of_friends}{$low} = true;
            $viewer->{mutual_friends}{$low}++ if $low ne $viewer->{id};
        }
    }
    return $viewer;
}

sub can_view ($viewer, $post) {
    my $author_id = $post->{author_id};
    return true if $author_id eq $viewer->{id};
    return false if defined $post->{window_ended_at} || !$viewer->{has_posted_today} || $viewer->{blocked}{$author_id};

    my $visibility = $post->{visibility};
    return true                            if $visibility eq 'public';
    return !!$viewer->{friends}{$author_id} if $visibility eq 'friends';
    return !!($viewer->{friends}{$author_id} || $viewer->{friends_of_friends}{$author_id})
        if $visibility eq 'friends_of_friends';
    return false;
}

sub can_view_post ($db, $viewer_id, $post_id) {
    my $post   = get_post_with_author($db, $post_id)  or return false;
    my $viewer = load_viewer_context($db, $viewer_id) or return false;
    return can_view($viewer, $post->{post});
}

sub get_friend_ids ($db, $user_id) {
    my $friendships = $db->query(
        q{SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id = $1 OR user_high_id = $1},
        $user_id,
    )->hashes;
    return map { $_->{user_low_id} eq $user_id ? $_->{user_high_id} : $_->{user_low_id} } @$friendships;
}

sub get_blocked_user_ids ($db, $user_id) {
    my $ids = $db->query(<<~'SQL', $user_id)->arrays;
          SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END
          FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
        SQL
    return { map { $_->[0] => true } @$ids };
}

sub post_location ($post) {
    my $location = { kind => $post->{location_kind} };
    $location->{latitude}  = 0 + $post->{latitude}  if defined $post->{latitude};
    $location->{longitude} = 0 + $post->{longitude} if defined $post->{longitude};
    $location->{locality}  = $post->{locality}      if defined $post->{locality};
    return $location;
}

sub post_response ($post, $user) {
    my $posted_at = moment($post->{posted_at});
    my $deadline  = moment($post->{on_time_deadline});
    my $on_time   = $posted_at->compare($deadline) <= 0;
    return {
        id              => $post->{id},
        author          => relationship_user_response($user),
        postingWindowId => $post->{posting_window_id},
        imageUrl        => signed_image_url($user, $post->{image_id}),
        caption         => $post->{caption},
        visibility      => $post->{visibility},
        location        => post_location($post),
        postedAt        => rfc3339($post->{posted_at}),
        onTime          => $on_time,
        lateSeconds     => $on_time ? 0 : int($deadline->delta_microseconds($posted_at) / 1_000_000),
    };
}

1;
