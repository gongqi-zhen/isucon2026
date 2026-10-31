package Isureal::Relationship;
use v5.44;
use utf8;

use Exporter 'import';
use Isureal::APIError;
use Isureal::Util qw(new_uuid min_max);

our @EXPORT_OK = qw(
    get_friend_request
    begin_relationship_tx
    is_blocked
    insert_friendship
    require_relationship_change
);

sub get_friend_request ($db, $id) {
    return $db->query(
        q{SELECT id, requester_id, recipient_id, status, created_at, updated_at FROM friend_requests WHERE id=$1},
        $id,
    )->hash;
}

# トランザクションを開始し、関係する 2 人のユーザーを ID 順にロックする
sub begin_relationship_tx ($db, $actor, $target) {
    Isureal::APIError->throw(400, 'invalid_relationship') if !defined $target || $target eq '' || $actor eq $target;
    my $tx = $db->begin;
    for my $id (min_max($actor, $target)) {
        $db->query(q{SELECT id FROM users WHERE id=$1 AND status='active' FOR NO KEY UPDATE}, $id)->array
            or Isureal::APIError->throw(404, 'relationship_not_found');
    }
    return $tx;
}

sub is_blocked ($db, $actor, $target) {
    return $db->query(
        q{SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))},
        $actor, $target,
    )->array->[0];
}

sub insert_friendship ($db, $request) {
    $db->query(
        q{INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id) VALUES ($1,$2,$3,$4)},
        new_uuid(), min_max($request->{requester_id}, $request->{recipient_id}), $request->{id},
    );
}

sub require_relationship_change ($results, $status, $code) {
    Isureal::APIError->throw($status, $code) if $results->rows == 0;
}

1;
