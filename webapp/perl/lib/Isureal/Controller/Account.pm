package Isureal::Controller::Account;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use Isureal::APIError;
use Isureal::Session qw(create_login_session_tx invalidate_login_session);
use Isureal::User qw(USER_COLUMNS valid_username valid_user_text valid_region get_user session_user_response me_response);
use Isureal::Util qw(NO_ROWS new_uuid sha256_hex);

sub register ($c) {
    my $input = $c->json_body(username => 'string', displayName => 'string', region => 'string');
    $c->stash('isureal.username' => $input->{username});
    unless (valid_username($input->{username}) && valid_user_text($input->{displayName}, 1, 100) && valid_region($input->{region})) {
        Isureal::APIError->throw(400, 'invalid_registration');
    }
    my $secret_key = new_uuid();
    my $db         = $c->db;
    my $tx         = $db->begin;
    # ユーザー名が一意である必要があります。
    $db->query(q{LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE});
    my $exists = $db->query(q{SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)}, $input->{username})->array->[0];
    Isureal::APIError->throw(409, 'username_taken') if $exists;

    my $user_id = new_uuid();
    $db->query(
        q{INSERT INTO users (id, username, secret_key_hash, display_name, region) VALUES ($1, $2, $3, $4, $5)},
        $user_id, $input->{username}, sha256_hex($secret_key), $input->{displayName}, $input->{region},
    );
    my $user = get_user($db, $user_id) // die NO_ROWS;
    $tx->commit;
    $c->render(status => 201, json => { secretKey => $secret_key, user => session_user_response($user) });
}

sub login ($c) {
    my $input = $c->json_body(username => 'string', secretKey => 'string');
    $c->stash('isureal.username' => $input->{username});
    Isureal::APIError->throw(401, 'invalid_credentials') unless valid_username($input->{username});

    my $db   = $c->db;
    my $tx   = $db->begin;
    my $user = $db->query(
        'SELECT ' . join(', ', USER_COLUMNS) . q{ FROM users WHERE username = $1 FOR NO KEY UPDATE},
        $input->{username},
    )->hash or Isureal::APIError->throw(401, 'invalid_credentials');
    if ($user->{status} ne 'active' || $user->{secret_key_hash} ne sha256_hex($input->{secretKey})) {
        Isureal::APIError->throw(401, 'invalid_credentials');
    }
    my $session = create_login_session_tx($db, $user->{id});
    $tx->commit;
    $c->render(json => { token => $session->{token}, user => session_user_response($user) });
}

sub logout ($c) {
    my $session = $c->current_session or Isureal::APIError->throw(401, 'invalid_session');
    invalidate_login_session($c->db, $session->@{qw(user_id token_hash)});
    $c->no_content;
}

sub me ($c) {
    my $session = $c->current_session or Isureal::APIError->throw(401, 'invalid_session');
    my $user    = get_user($c->db, $session->{user_id}) or Isureal::APIError->throw(404, 'account_not_found');
    $c->render(json => me_response($user));
}

sub update_me ($c) {
    my $session = $c->current_session or Isureal::APIError->throw(401, 'invalid_session');
    my $input   = $c->json_body(displayName => '?string', bio => '?string', region => '?string');
    my ($display_name, $bio, $region) = $input->@{qw(displayName bio region)};
    if (   (!defined $display_name && !defined $bio && !defined $region)
        || (defined $display_name && !valid_user_text($display_name, 1, 100))
        || (defined $bio          && !valid_user_text($bio, 0, 500))
        || (defined $region       && !valid_region($region)))
    {
        Isureal::APIError->throw(400, 'invalid_profile');
    }
    my $db = $c->db;
    my $tx = $db->begin;
    if (defined $display_name) {
        $db->query(q{UPDATE users SET display_name = $2, updated_at = clock_timestamp() WHERE id = $1}, $session->{user_id}, $display_name);
    }
    if (defined $bio) {
        $db->query(q{UPDATE users SET bio = $2, updated_at = clock_timestamp() WHERE id = $1}, $session->{user_id}, $bio);
    }
    if (defined $region) {
        $db->query(q{UPDATE users SET region = $2, updated_at = clock_timestamp() WHERE id = $1}, $session->{user_id}, $region);
    }
    my $user = get_user($db, $session->{user_id}) or Isureal::APIError->throw(404, 'account_not_found');
    $tx->commit;
    $c->render(json => me_response($user));
}

sub delete_account ($c) {
    my $user_id = $c->current_session->{user_id};
    my $db      = $c->db;
    my $tx      = $db->begin;
    $db->query(q{SELECT id FROM users WHERE id=$1 AND status='active' FOR NO KEY UPDATE}, $user_id)->array
        or Isureal::APIError->throw(404, 'account_not_found');
    $db->query(q{UPDATE users SET status='deleted', deleted_at=clock_timestamp(), updated_at=clock_timestamp() WHERE id=$1}, $user_id);
    $db->query(q{UPDATE login_sessions SET active=false, invalidated_at=clock_timestamp(), reason='account_deletion' WHERE user_id=$1 AND active=true}, $user_id);
    $tx->commit;
    $c->no_content;
}

1;
