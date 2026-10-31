package Isureal::Controller::Admin;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use Isureal::APIError;
use Isureal::AdminSession qw(create_admin_session invalidate_admin_session);
use Isureal::PostingWindow qw(get_posting_window);
use Isureal::User qw(valid_username valid_region);
use Isureal::Util qw(NO_ROWS new_uuid sha256_hex moment parse_date rfc3339);

sub login ($c) {
    my $input = $c->json_body(username => 'string', password => 'string');
    Isureal::APIError->throw(401, 'invalid_credentials') unless valid_username($input->{username});
    my $db            = $c->db;
    my $administrator = $db->query(q{SELECT id, password_hash FROM administrators WHERE username = $1}, $input->{username})->hash
        or Isureal::APIError->throw(401, 'invalid_credentials');
    if ($administrator->{password_hash} ne sha256_hex($input->{password})) {
        Isureal::APIError->throw(401, 'invalid_credentials');
    }
    my $session = create_admin_session($db, $administrator->{id});
    $c->render(json => { token => $session->{token}, administratorId => $session->{administrator_id} });
}

sub logout ($c) {
    my $session = $c->current_admin_session or Isureal::APIError->throw(401, 'invalid_administrator_session');
    invalidate_admin_session($c->db, $session->@{qw(administrator_id token_hash)});
    $c->no_content;
}

sub certify_user ($c) {
    my $results = $c->db->query(
        q{UPDATE users SET account_type='certified_public', updated_at=clock_timestamp() WHERE id=$1 AND status='active' AND account_type='regular'},
        $c->param('userId'),
    );
    Isureal::APIError->throw(409, 'certification_conflict') if $results->rows == 0;
    $c->no_content;
}

# 2026-08-26: 認定されたユーザーを戻すことは禁止する
sub delete_user_certification ($c) {
    Isureal::APIError->throw(409, 'certification_irreversible');
}

sub open_posting_window ($c) {
    my $input      = $c->json_body(region => 'string', localDate => 'string', onTimeSeconds => '?int');
    my $region     = $input->{region};
    my $local_date = parse_date($input->{localDate});
    Isureal::APIError->throw(400, 'invalid_posting_window') if !valid_region($region) || !$local_date;
    my $seconds = $input->{onTimeSeconds} // 24 * 60 * 60;

    my $db = $c->db;
    my $tx = $db->begin;
    $db->query(q{LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE});
    my $conflict = $db->query(
        q{SELECT EXISTS(SELECT 1 FROM posting_windows WHERE region = $1 AND local_date >= $2)},
        $region, $local_date->strftime('%Y-%m-%d'),
    )->array->[0];
    Isureal::APIError->throw(409, 'posting_window_conflict') if $conflict;

    my $starts_at = moment($db->query(q{SELECT clock_timestamp()})->array->[0]);
    $db->query(q{UPDATE posting_windows SET ended_at = $2 WHERE region = $1 AND ended_at IS NULL}, $region, $starts_at->to_string);
    my $window_id = new_uuid();
    $db->query(<<~'SQL', $window_id, $region, $local_date->strftime('%Y-%m-%d'), $starts_at->to_string, $starts_at->plus_seconds($seconds)->to_string);
           INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
           VALUES ($1, $2, $3, $4, $5)
        SQL
    $db->query(<<~'SQL', $region, $window_id, $starts_at->to_string);
           INSERT INTO notifications (id, user_id, posting_window_id, created_at)
           SELECT gen_random_uuid()::text, id, $2, $3 FROM users WHERE region = $1 AND status = 'active'
        SQL
    my $window = get_posting_window($db, $window_id) // die NO_ROWS;
    $tx->commit;
    $c->render(status => 201, json => {
        id             => $window->{id},
        region         => $window->{region},
        localDate      => $window->{local_date},
        startsAt       => rfc3339($window->{starts_at}),
        onTimeDeadline => rfc3339($window->{on_time_deadline}),
    });
}

1;
