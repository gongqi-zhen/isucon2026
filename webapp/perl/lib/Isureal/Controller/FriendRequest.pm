package Isureal::Controller::FriendRequest;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use List::Util qw(any);
use Isureal::APIError;
use Isureal::Post qw(get_friend_ids);
use Isureal::Relationship qw(get_friend_request begin_relationship_tx is_blocked insert_friendship);
use Isureal::Util qw(NO_ROWS new_uuid rfc3339);

sub friend_request_response ($request) {
    return {
        relationId => $request->{id},
        status     => $request->{status},
        updatedAt  => rfc3339($request->{updated_at}),
    };
}

sub create ($c) {
    my $recipient_id = $c->json_body(recipientId => 'string')->{recipientId};
    my $actor        = $c->current_session->{user_id};
    my $db           = $c->db;
    my $tx           = begin_relationship_tx($db, $actor, $recipient_id);
    Isureal::APIError->throw(409, 'relationship_conflict') if is_blocked($db, $actor, $recipient_id);
    Isureal::APIError->throw(409, 'relationship_conflict') if any { $_ eq $recipient_id } get_friend_ids($db, $actor);

    my $existing = $db->query(<<~'SQL', $actor, $recipient_id)->array;
           SELECT id FROM friend_requests WHERE status='pending' AND
            ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))
        SQL
    my $request_id;
    if ($existing) {
        my $request = get_friend_request($db, $existing->[0]) // die NO_ROWS;
        Isureal::APIError->throw(409, 'relationship_conflict') if $request->{requester_id} eq $actor;
        $db->query(q{UPDATE friend_requests SET status='accepted', updated_at=clock_timestamp() WHERE id=$1}, $request->{id});
        insert_friendship($db, $request);
        $request_id = $request->{id};
    }
    else {
        $request_id = new_uuid();
        $db->query(
            q{INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at) VALUES ($1,$2,$3,'pending',clock_timestamp(),clock_timestamp())},
            $request_id, $actor, $recipient_id,
        );
    }
    my $request = get_friend_request($db, $request_id) // die NO_ROWS;
    $tx->commit;
    $c->render(status => 201, json => friend_request_response($request));
}

my %STATUS_FOR_ACTION = (accept => 'accepted', reject => 'rejected', cancel => 'cancelled');

sub transition ($c) {
    my $status = $STATUS_FOR_ACTION{$c->param('requestAction')}
        or Isureal::APIError->throw(403, 'relationship_action_forbidden');
    my $request_id = $c->param('requestId');
    my $actor      = $c->current_session->{user_id};
    my $db         = $c->db;
    my $request    = get_friend_request($db, $request_id) or Isureal::APIError->throw(404, 'relationship_not_found');
    if (($status eq 'cancelled' && $actor ne $request->{requester_id}) || ($status ne 'cancelled' && $actor ne $request->{recipient_id})) {
        Isureal::APIError->throw(403, 'relationship_action_forbidden');
    }
    my $tx = begin_relationship_tx($db, $request->@{qw(requester_id recipient_id)});
    $request = get_friend_request($db, $request_id) // die NO_ROWS;
    Isureal::APIError->throw(403, 'relationship_action_forbidden') if $request->{status} ne 'pending';
    if ($status eq 'accepted') {
        Isureal::APIError->throw(409, 'relationship_conflict') if is_blocked($db, $request->@{qw(requester_id recipient_id)});
        insert_friendship($db, $request);
    }
    $db->query(q{UPDATE friend_requests SET status=$2, updated_at=clock_timestamp() WHERE id=$1}, $request->{id}, $status);
    $request = get_friend_request($db, $request->{id}) // die NO_ROWS;
    $tx->commit;
    $c->render(json => friend_request_response($request));
}

1;
