package Isureal::Controller::ProfileImage;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use DBD::Pg qw(PG_BYTEA);
use Isureal::APIError;
use Isureal::JPEG qw(decode_jpeg);
use Isureal::Util qw(new_uuid);

sub update ($c) {
    if (($c->req->headers->content_type // '') ne 'image/jpeg') {
        Isureal::APIError->throw(415, 'unsupported_media_type');
    }
    my $data    = $c->req->body;
    my $decoded = decode_jpeg($data);
    unless ($decoded) {
        $c->stash('isureal.abuse' => 'invalid_image');
        Isureal::APIError->throw(400, 'invalid_profile_image');
    }
    Isureal::APIError->throw(400, 'invalid_profile_image') if $decoded->{width} > 512 || $decoded->{height} > 512;

    my $user_id = $c->current_session->{user_id};
    my $db      = $c->db;
    my $tx      = $db->begin;
    my $row     = $db->query(q{SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE}, $user_id)->array
        or Isureal::APIError->throw(404, 'account_not_found');
    my $old_id   = $row->[0];
    my $image_id = new_uuid();
    $db->query(
        q{INSERT INTO image_objects(id,owner_user_id,data,media_type) VALUES ($1,$2,$3,'image/jpeg')},
        $image_id, $user_id, { type => PG_BYTEA, value => $data },
    );
    $db->query(q{UPDATE users SET profile_image_id=$2,updated_at=clock_timestamp() WHERE id=$1}, $user_id, $image_id);
    $db->query(q{DELETE FROM image_objects WHERE id=$1}, $old_id) if defined $old_id;
    $tx->commit;
    $c->no_content;
}

sub remove ($c) {
    my $user_id = $c->current_session->{user_id};
    my $db      = $c->db;
    my $tx      = $db->begin;
    my $row     = $db->query(q{SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE}, $user_id)->array
        or Isureal::APIError->throw(404, 'account_not_found');
    my $image_id = $row->[0] // Isureal::APIError->throw(404, 'account_not_found');
    $db->query(q{UPDATE users SET profile_image_id=NULL,updated_at=clock_timestamp() WHERE id=$1}, $user_id);
    $db->query(q{DELETE FROM image_objects WHERE id=$1}, $image_id);
    $tx->commit;
    $c->no_content;
}

1;
