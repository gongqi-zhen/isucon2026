package Isureal::Controller::Session;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use Isureal::APIError;
use Isureal::Session qw(get_session_from_header);

sub authenticate ($c) {
    my $session = get_session_from_header($c)
        or Isureal::APIError->throw(401, 'invalid_session');
    $c->stash('isureal.session' => $session);
    return 1;
}

sub check_region ($c) {
    my $session = $c->current_session or Isureal::APIError->throw(401, 'invalid_session');
    Isureal::APIError->throw(403, 'region_mismatch') if $session->{region} ne $c->region;
    return 1;
}

1;
