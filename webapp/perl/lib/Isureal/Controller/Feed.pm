package Isureal::Controller::Feed;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use Isureal::APIError;
use Isureal::Post qw(select_posts_with_author load_viewer_context can_view post_response);
use Isureal::Util qw(moment placeholders);

sub show ($c) {
    my $feed             = $c->param('feed');
    my $before_score     = $c->query_int('beforeScore');
    my $before_posted_at = $c->query_time('beforePostedAt');
    my $before_id        = $c->query_string('beforeId');
    Isureal::APIError->throw(400, 'invalid_post') unless $feed =~ /\A(?:friends|friends-of-friends|public|following)\z/;
    Isureal::APIError->throw(400, 'invalid_pagination') if defined $before_posted_at xor $before_id ne '';
    my $ranked = $feed eq 'friends-of-friends';
    Isureal::APIError->throw(400, 'invalid_pagination') if $ranked && (defined $before_score xor $before_id ne '');

    my $db     = $c->db;
    my $viewer = load_viewer_context($db, $c->current_session->{user_id})
        or return $c->render(json => { items => [], hasMore => false });
    my %following;
    if ($feed eq 'following') {
        my $followee_ids = $db->query(q{SELECT followee_id FROM follows WHERE follower_id = $1}, $viewer->{id})->arrays;
        $following{$_->[0]} = true for @$followee_ids;
    }
    my ($cursor_posted_at, $cursor_id) = $ranked ? (undef, '') : ($before_posted_at && $before_posted_at->to_string, $before_id);
    my $posts = select_posts_with_author($db, <<~'SQL', $viewer->{id}, $cursor_posted_at, $cursor_id);
           AND p.author_id <> $1 AND w.ended_at IS NULL
           AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
           ORDER BY p.posted_at DESC, p.id DESC
        SQL
    my @candidates = grep { can_view($viewer, $_->{post}) && matches_feed($viewer, \%following, $_->{post}, $feed) } @$posts;
    my $scores = {};
    if ($ranked) {
        $scores     = recommendation_scores($db, $viewer, \@candidates);
        @candidates = sort { $scores->{$b->{post}{id}} <=> $scores->{$a->{post}{id}} } @candidates;
    }
    my @items;
    for my $post (@candidates) {
        my $score = $scores->{$post->{post}{id}};
        next if $ranked && defined $before_score && !ranked_before($score, $post->{post}, $before_score, $before_posted_at, $before_id);
        my $item = post_response($post->{post}, $post->{author});
        $item->{score} = $score if $ranked;
        push @items, $item;
        last if @items == 51;
    }
    my $has_more = @items > 50;
    splice @items, 50 if $has_more;
    $c->render(json => { items => \@items, hasMore => $has_more });
}

sub ranked_before ($score, $post, $before_score, $before_posted_at, $before_id) {
    return $score < $before_score if $score != $before_score;
    my $order = moment($post->{posted_at})->compare($before_posted_at);
    return $order < 0 if $order != 0;
    return $post->{id} lt $before_id;
}

sub recommendation_scores ($db, $viewer, $posts) {
    my %scores;
    return \%scores unless @$posts;
    my @author_ids = map { $_->{post}{author_id} } @$posts;
    my @post_ids   = map { $_->{post}{id} } @$posts;

    my $in          = placeholders(scalar @author_ids);
    my $friendships = $db->query(
        "SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN ($in) OR user_high_id IN ($in)",
        @author_ids,
    )->hashes;
    my $mutual_friends = $viewer->{mutual_friends};
    my %connections;
    for my $friendship (@$friendships) {
        my ($low, $high) = $friendship->@{qw(user_low_id user_high_id)};
        $connections{$low}  += $mutual_friends->{$high} // 0;
        $connections{$high} += $mutual_friends->{$low}  // 0;
    }

    $in = placeholders(scalar @post_ids);
    my $engagements = $db->query(<<~"SQL", @post_ids)->hashes;
          SELECT r.post_id, r.reactor_id AS user_id FROM realmojis r
          JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
          WHERE r.post_id IN ($in) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
          UNION
          SELECT c.post_id, c.author_id AS user_id FROM comments c
          JOIN users u ON u.id = c.author_id AND u.status = 'active'
          WHERE c.post_id IN ($in) AND c.deleted_at IS NULL
        SQL
    my %friend_engagements;
    for my $engagement (@$engagements) {
        $friend_engagements{$engagement->{post_id}}++ if $viewer->{friends}{$engagement->{user_id}};
    }

    for my $post (@$posts) {
        my ($id, $author_id) = $post->{post}->@{qw(id author_id)};
        my $connections = $connections{$author_id} // 0;
        $scores{$id} = 10 * ($friend_engagements{$id} // 0) + 3 * ($mutual_friends->{$author_id} // 0) + ($connections < 30 ? $connections : 30);
    }
    return \%scores;
}

sub matches_feed ($viewer, $following, $post, $feed) {
    my $author_id = $post->{author_id};
    return $post->{visibility} eq 'public' if $feed eq 'public';
    return !!$following->{$author_id}       if $feed eq 'following';
    return !!$viewer->{friends}{$author_id} if $feed eq 'friends';
    return !$viewer->{friends}{$author_id} && !!$viewer->{friends_of_friends}{$author_id} if $feed eq 'friends-of-friends';
    return false;
}

1;
