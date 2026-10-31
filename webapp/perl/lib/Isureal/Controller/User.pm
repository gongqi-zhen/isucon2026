package Isureal::Controller::User;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use List::Util qw(any);
use Unicode::Normalize qw(NFC);
use Isureal::APIError;
use Isureal::Post qw(get_blocked_user_ids get_friend_ids);
use Isureal::Relationship qw(is_blocked);
use Isureal::User qw(USER_COLUMNS valid_user_text get_user with_image_key relationship_user_response);

sub normalize_search_text ($value) {
    return NFC(fc(NFC($value)));
}

sub search ($c) {
    my $q               = $c->query_string('q');
    my $before_username = $c->query_string('beforeUsername');
    my $before_id       = $c->query_string('beforeId');
    Isureal::APIError->throw(400, 'invalid_relationship') unless valid_user_text($q, 1, 100);
    Isureal::APIError->throw(400, 'invalid_pagination') if $before_username eq '' xor $before_id eq '';

    my $db     = $c->db;
    my $viewer = $c->current_session->{user_id};
    my $users  = $db->query(
        'SELECT ' . join(', ', USER_COLUMNS)
            . q{ FROM users WHERE status='active' AND id<>$1 AND ($2='' OR (username,id)>($2,$3)) ORDER BY username, id},
        $viewer, $before_username, $before_id,
    )->hashes;
    my $blocked = get_blocked_user_ids($db, $viewer);
    my $query   = normalize_search_text($q);
    my @items;
    for my $user (@$users) {
        next if index(normalize_search_text($user->{username}), $query) < 0
            && index(normalize_search_text($user->{display_name}), $query) < 0;
        next if $blocked->{$user->{id}};
        with_image_key($user);
        $user->{bio} = '' if $user->{account_type} ne 'certified_public';
        push @items, relationship_user_response($user);
        last if @items == 51;
    }
    my $has_more = @items > 50;
    splice @items, 50 if $has_more;
    $c->render(json => { items => \@items, hasMore => $has_more });
}

sub show ($c) {
    my $db     = $c->db;
    my $viewer = $c->current_session->{user_id};
    my $user   = get_user($db, $c->param('userId')) or Isureal::APIError->throw(404, 'relationship_not_found');
    Isureal::APIError->throw(404, 'relationship_not_found') if is_blocked($db, $viewer, $user->{id});
    my @friends = get_friend_ids($db, $viewer);
    if ($user->{account_type} ne 'certified_public' && !any { $_ eq $user->{id} } @friends) {
        $user->{bio} = '';
    }
    $c->render(json => relationship_user_response($user));
}

1;
