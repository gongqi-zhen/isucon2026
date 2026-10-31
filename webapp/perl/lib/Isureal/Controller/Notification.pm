package Isureal::Controller::Notification;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller', -async_await;

use Mojo::JSON qw(encode_json);
use Mojo::Promise;
use Isureal::User qw(USER_COLUMNS);
use Isureal::Util qw(rfc3339);

use constant POLL_INTERVAL => 0.5;

# 通知が届くまで接続を保持するので、DB への問い合わせは非同期で行う
async sub stream ($c) {
    delete $c->stash->{'isureal.db'};
    my $session  = $c->current_session;
    my $user_id  = $session->{user_id};
    my $async_db = $c->async_db;
    my $log      = $c->app->log;

    my $connected_at;
    try {
        $connected_at = (await $async_db->query_p(q{SELECT clock_timestamp()}))->array->[0];
    }
    catch ($e) {
        return $c->render_error($e);
    }

    my $closed = false;
    $c->on(finish => sub { $closed = true });
    $c->inactivity_timeout(3600);
    $c->res->headers->content_type('text/event-stream');
    $c->write_chunk;

    try {
        until ($closed) {
            my $user = (await $async_db->query_p(
                'SELECT ' . join(', ', USER_COLUMNS) . q{ FROM users WHERE id = $1 AND status = 'active'},
                $user_id,
            ))->hash;
            last unless $user && $user->{region} eq $session->{region};

            my $notification = (await $async_db->query_p(<<~'SQL', $user_id, $connected_at))->hash;
                SELECT n.id, n.posting_window_id, w.region, n.created_at
                FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
                WHERE n.user_id = $1 AND n.created_at > $2 AND w.ended_at IS NULL
                ORDER BY n.created_at, n.id LIMIT 1
                SQL
            if ($notification) {
                my $data = encode_json({
                    id              => $notification->{id},
                    postingWindowId => $notification->{posting_window_id},
                    region          => $notification->{region},
                    createdAt       => rfc3339($notification->{created_at}),
                });
                $c->write_chunk("data: $data\n\n") unless $closed;
                last;
            }
            await Mojo::Promise->timer(POLL_INTERVAL);
        }
    }
    catch ($e) {
        $log->error("failed to get notification: $e") unless $closed;
    }
    $c->write_chunk('') unless $closed;
}

1;
