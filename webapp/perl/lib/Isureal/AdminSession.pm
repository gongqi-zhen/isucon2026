package Isureal::AdminSession;
use v5.44;
use utf8;

use Exporter 'import';
use Isureal::APIError;
use Isureal::Util qw(NO_ROWS new_uuid sha256_hex);

our @EXPORT_OK = qw(get_admin_session_from_header create_admin_session invalidate_admin_session);

sub get_admin_session_from_header ($c) {
    my $auth_header = $c->req->headers->authorization // '';
    return undef if $auth_header eq '';
    $auth_header =~ /\ABearer (.*)\z/s
        or Isureal::APIError->throw(401, 'invalid_administrator_session');
    return $c->db->query(<<~'SQL', sha256_hex($1))->hash;
          SELECT administrator_id, token_hash, created_at
          FROM administrator_sessions
          WHERE token_hash = $1 AND active = true
        SQL
}

sub create_admin_session ($db, $administrator_id) {
    my $tx = $db->begin;
    my ($session_id, $token) = (new_uuid(), new_uuid());
    $db->query(<<~'SQL', $session_id, $administrator_id, sha256_hex($token));
          INSERT INTO administrator_sessions (id, administrator_id, token_hash)
          VALUES ($1, $2, $3)
        SQL
    my $session = get_admin_session($db, $session_id);
    $tx->commit;
    $session->{token} = $token;
    return $session;
}

sub get_admin_session ($db, $session_id) {
    return $db->query(
        q{SELECT administrator_id, token_hash, created_at FROM administrator_sessions WHERE id = $1},
        $session_id,
    )->hash // die "failed to get admin session: ${\NO_ROWS}";
}

sub invalidate_admin_session ($db, $administrator_id, $token_hash) {
    $db->query(<<~'SQL', $administrator_id, $token_hash);
          UPDATE administrator_sessions
          SET active = false, invalidated_at = clock_timestamp()
          WHERE administrator_id = $1 AND token_hash = $2 AND active = true
        SQL
}

1;
