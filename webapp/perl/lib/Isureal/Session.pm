package Isureal::Session;
use v5.44;
use utf8;

use Exporter 'import';
use Isureal::APIError;
use Isureal::Util qw(NO_ROWS new_uuid sha256_hex);

our @EXPORT_OK = qw(
    get_session_from_header
    invalidate_login_session
    create_login_session
    create_login_session_tx
);

sub get_session_from_header ($c) {
    my $auth_header = $c->req->headers->authorization // '';
    return undef if $auth_header eq '';
    unless ($auth_header =~ /\ABearer (.*)\z/s) {
        $c->app->log->warn('Invalid authorization header format(not started with Bearer)');
        Isureal::APIError->throw(401, 'invalid_session');
    }
    my $token_hash = sha256_hex($1);
    return $c->db->query(<<~'SQL', $token_hash)->hash;
        SELECT s.user_id, s.token_hash, s.created_at, u.region
        FROM login_sessions s JOIN users u ON u.id = s.user_id
        WHERE s.token_hash = $1
          AND s.active = true AND u.status = 'active'
        SQL
}

sub invalidate_login_session ($db, $user_id, $token_hash) {
    my $tx = $db->begin;

    $db->query(q{SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE}, $user_id)->array
        or die "failed to lock user: ${\NO_ROWS}";

    $db->query(<<~'SQL', $user_id, $token_hash);
        UPDATE login_sessions
        SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
        WHERE user_id = $1 AND active = true AND token_hash = $2
        SQL

    $tx->commit;
}

# 2026-09-03: リファクタリング後にこの関数の利用がないようです。消すのはいかがでしょうか
sub create_login_session ($db, $user_id) {
    my $tx      = $db->begin;
    my $session = create_login_session_tx($db, $user_id);
    $tx->commit;
    return $session;
}

sub create_login_session_tx ($db, $user_id) {
    my $random_token = new_uuid();
    my $token_hash   = sha256_hex($random_token);
    $db->query(q{SELECT id FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE}, $user_id)->array
        or die "failed to lock user: ${\NO_ROWS}";
    $db->query(<<~'SQL', $user_id);
          UPDATE login_sessions
          SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
          WHERE user_id = $1 AND active = true
        SQL
    my $session_id = new_uuid();
    $db->query(<<~'SQL', $session_id, $user_id, $token_hash);
          INSERT INTO login_sessions (id, user_id, token_hash)
          VALUES ($1, $2, $3)
        SQL
    my $session = get_login_session($db, $session_id);
    $session->{token} = $random_token;
    return $session;
}

sub get_login_session ($db, $session_id) {
    return $db->query(q{SELECT user_id, token_hash, created_at FROM login_sessions WHERE id = $1}, $session_id)->hash
        // die "failed to get session: ${\NO_ROWS}";
}

1;
