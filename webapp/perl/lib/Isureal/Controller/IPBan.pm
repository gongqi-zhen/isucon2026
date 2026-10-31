package Isureal::Controller::IPBan;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use Isureal::APIError;
use Isureal::IPBan qw(client_ip);

sub check ($c) {
    Isureal::APIError->throw(403, 'ip_banned') if $c->ip_bans->banned($c->pg, client_ip($c));
    return 1;
}

1;
