package Isureal::Controller::PostingWindow;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use Isureal::Util qw(moment now_utc rfc3339);

sub current ($c) {
    my $state = $c->db->query(<<~'SQL', $c->current_session->{user_id})->hash;
           SELECT w.id, w.region, w.local_date, w.starts_at, w.on_time_deadline, w.ended_at,
                  count(p.id) FILTER (WHERE p.deleted_at IS NULL) AS active,
                  count(p.id) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
           FROM users u JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
           LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date
           LEFT JOIN posts p ON p.author_id = u.id AND p.posting_window_id = posted_window.id
           WHERE u.id = $1
           GROUP BY w.id
        SQL
    unless ($state) {
        return $c->render(json => { window => undef, hasPosted => false, canPost => false, deletionCount => 0 });
    }
    $c->render(json => {
        window => {
            id             => $state->{id},
            region         => $state->{region},
            localDate      => $state->{local_date},
            startsAt       => rfc3339($state->{starts_at}),
            onTimeDeadline => rfc3339($state->{on_time_deadline}),
        },
        hasPosted     => $state->{active} > 0,
        canPost       => $state->{active} == 0 && $state->{deleted} < 2 && !now_utc()->is_before(moment($state->{starts_at})),
        deletionCount => 0 + $state->{deleted},
    });
}

1;
