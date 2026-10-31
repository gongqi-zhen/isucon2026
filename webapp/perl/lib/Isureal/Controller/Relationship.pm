package Isureal::Controller::Relationship;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use Isureal::APIError;
use Isureal::Relationship qw(begin_relationship_tx is_blocked require_relationship_change);
use Isureal::User qw(get_user extract_user user_columns_as relationship_user_response);
use Isureal::Util qw(NO_ROWS new_uuid min_max rfc3339);

my %RELATION_QUERIES = (
    'friends'           => q{SELECT id, user_low_id AS first_id, user_high_id AS second_id, updated_at FROM friendships WHERE user_low_id=$1 OR user_high_id=$1},
    'following'         => q{SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE follower_id=$1},
    'followers'         => q{SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE followee_id=$1},
    'incoming-requests' => q{SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE recipient_id=$1 AND status='pending'},
    'outgoing-requests' => q{SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE requester_id=$1 AND status='pending'},
);

sub list ($c) {
    my $kind              = $c->param('kind');
    my $before_updated_at = $c->query_time('beforeUpdatedAt');
    my $before_id         = $c->query_string('beforeId');
    Isureal::APIError->throw(400, 'invalid_pagination') if defined $before_updated_at xor $before_id ne '';
    my $query = $RELATION_QUERIES{$kind} or Isureal::APIError->throw(400, 'invalid_relationship');

    my $actor   = $c->current_session->{user_id};
    my $records = $c->db->query(<<~"SQL", $actor, $before_updated_at && $before_updated_at->to_string, $before_id)->hashes;
           SELECT relations.id, relations.updated_at, ${\user_columns_as('u', 'user')}
           FROM ($query) relations
           JOIN users u ON u.status = 'active'
                       AND u.id = CASE WHEN relations.first_id = \$1 THEN relations.second_id ELSE relations.first_id END
           WHERE (\$2::timestamptz IS NULL OR (relations.updated_at, relations.id) < (\$2, \$3))
           ORDER BY relations.updated_at DESC, relations.id DESC LIMIT 51
        SQL
    my $status = $kind eq 'incoming-requests' || $kind eq 'outgoing-requests' ? 'pending' : $kind;
    my @items;
    for my $record (@$records) {
        my $user = extract_user($record, 'user');
        $user->{bio} = '' if $user->{account_type} ne 'certified_public' && $kind ne 'friends';
        push @items, {
            relationId => $record->{id},
            status     => $status,
            updatedAt  => rfc3339($record->{updated_at}),
            user       => relationship_user_response($user),
        };
    }
    my $has_more = @items > 50;
    splice @items, 50 if $has_more;
    $c->render(json => { items => \@items, hasMore => $has_more });
}

sub delete_friendship ($c) {
    my ($actor, $target) = ($c->current_session->{user_id}, $c->param('userId'));
    my $db = $c->db;
    my $tx = begin_relationship_tx($db, $actor, $target);
    my $results = $db->query(q{DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2}, min_max($actor, $target));
    require_relationship_change($results, 409, 'relationship_conflict');
    $tx->commit;
    $c->no_content;
}

sub follow ($c) {
    my ($actor, $target_id) = ($c->current_session->{user_id}, $c->param('userId'));
    my $db     = $c->db;
    my $tx     = begin_relationship_tx($db, $actor, $target_id);
    my $target = get_user($db, $target_id) // die NO_ROWS;
    Isureal::APIError->throw(409, 'relationship_conflict') if $target->{account_type} ne 'certified_public';
    Isureal::APIError->throw(409, 'relationship_conflict') if is_blocked($db, $actor, $target->{id});
    my $exists = $db->query(
        q{SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id=$1 AND followee_id=$2)},
        $actor, $target->{id},
    )->array->[0];
    Isureal::APIError->throw(409, 'relationship_conflict') if $exists;
    $db->query(q{INSERT INTO follows (id, follower_id, followee_id) VALUES ($1,$2,$3)}, new_uuid(), $actor, $target->{id});
    $tx->commit;
    $c->no_content;
}

sub unfollow ($c) {
    my ($actor, $target) = ($c->current_session->{user_id}, $c->param('userId'));
    my $db      = $c->db;
    my $tx      = begin_relationship_tx($db, $actor, $target);
    my $results = $db->query(q{DELETE FROM follows WHERE follower_id=$1 AND followee_id=$2}, $actor, $target);
    require_relationship_change($results, 409, 'relationship_conflict');
    $tx->commit;
    $c->no_content;
}

sub block ($c) {
    my ($actor, $target) = ($c->current_session->{user_id}, $c->param('userId'));
    my $db     = $c->db;
    my $tx     = begin_relationship_tx($db, $actor, $target);
    my $exists = $db->query(
        q{SELECT EXISTS(SELECT 1 FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2)},
        $actor, $target,
    )->array->[0];
    Isureal::APIError->throw(409, 'relationship_conflict') if $exists;
    $db->query(q{INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES ($1,$2,$3)}, new_uuid(), $actor, $target);
    $db->query(q{DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2}, min_max($actor, $target));
    $db->query(q{DELETE FROM follows WHERE (follower_id=$1 AND followee_id=$2) OR (follower_id=$2 AND followee_id=$1)}, $actor, $target);
    $db->query(
        q{UPDATE friend_requests SET status='cancelled', updated_at=clock_timestamp() WHERE status='pending' AND ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))},
        $actor, $target,
    );
    $tx->commit;
    $c->no_content;
}

sub unblock ($c) {
    my ($actor, $target) = ($c->current_session->{user_id}, $c->param('userId'));
    my $db      = $c->db;
    my $tx      = begin_relationship_tx($db, $actor, $target);
    my $results = $db->query(q{DELETE FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2}, $actor, $target);
    require_relationship_change($results, 403, 'relationship_action_forbidden');
    $tx->commit;
    $c->no_content;
}

1;
