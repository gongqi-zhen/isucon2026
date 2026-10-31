package Isureal::IPBan;
use v5.44;
use utf8;

use Exporter 'import';
use Mojo::Util qw(steady_time);
use Time::HiRes qw(time);

our @EXPORT_OK = qw(client_ip);

use constant RELOAD_INTERVAL => 0.2;

sub new ($class) {
    return bless { snapshot => undef }, $class;
}

sub banned ($self, $pg, $ip) {
    my $snapshot = $self->{snapshot};
    if (!$snapshot || steady_time - $snapshot->{loaded_at} >= RELOAD_INTERVAL) {
        $snapshot = $self->{snapshot} = load_ip_bans($pg);
    }
    my $banned_until = $snapshot->{bans}{$ip};
    return defined $banned_until && time < $banned_until;
}

sub load_ip_bans ($pg) {
    my $rows = $pg->db->query(
        q{SELECT ip, extract(epoch FROM banned_until) FROM ip_bans WHERE banned_until > clock_timestamp()},
    )->arrays;
    return { loaded_at => steady_time, bans => { map { $_->[0] => $_->[1] } @$rows } };
}

sub client_ip ($c) {
    my $ip = $c->req->headers->every_header('X-Client-IP')->[0];
    return defined $ip && $ip ne '' ? $ip : $c->tx->remote_address;
}

1;
