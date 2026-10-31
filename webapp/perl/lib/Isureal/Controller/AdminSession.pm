package Isureal::Controller::AdminSession;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use Isureal::APIError;
use Isureal::AdminSession qw(get_admin_session_from_header);

sub authenticate ($c) {
    my $session = get_admin_session_from_header($c)
        or Isureal::APIError->throw(401, 'invalid_administrator_session');
    $c->stash('isureal.admin_session' => $session);
    return 1;
}

1;
