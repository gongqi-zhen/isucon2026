package Isureal::ActionLog;
use v5.44;
use utf8;
use Mojo::Base -base;

use Mojo::JSON qw(encode_json);
use Isureal::IPBan qw(client_ip);
use Isureal::User qw(valid_username);

my %LEVEL_RANKS = (debug => 0, info => 1, error => 2);

has min_level => 'debug';
has output    => sub { \*STDOUT };

sub record ($self, $c) {
    my $status = $c->res->code // 0;
    my $endpoint = $c->match->endpoint;
    my $action   = $endpoint && $endpoint->has_custom_name ? $endpoint->name : undef;
    my $level  = $status >= 500 ? 'error' : $c->req->method eq 'GET' && !($status == 403 && ($c->stash->{'isureal.abuse'} // '') eq 'invalid_signature') ? 'debug' : 'info';
    return if $LEVEL_RANKS{$level} < ($LEVEL_RANKS{$self->min_level} // 0);
    my $session  = $c->current_session;
    my %entry    = (
        action  => $action,
        ip      => client_ip($c),
        level   => $level,
        status  => 0 + $status,
        user_id => $session ? $session->{user_id} : undef,
    );
    if (defined $action && ($action eq 'login' || $action eq 'register')) {
        my $username = $c->stash->{'isureal.username'};
        $entry{username} = valid_username($username) ? $username : undef;
    }
    $entry{abuse} = $c->stash->{'isureal.abuse'} if defined $c->stash->{'isureal.abuse'};
    print { $self->output } encode_json(\%entry), "\n";
}

1;
