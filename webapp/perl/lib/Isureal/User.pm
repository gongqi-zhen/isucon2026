package Isureal::User;
use v5.44;
use utf8;

use Exporter 'import';
use Isureal::ImageSignature qw(image_signing_key signed_image_url);

our @EXPORT_OK = qw(
    USER_COLUMNS
    valid_username
    valid_user_text
    valid_region
    user_columns_as
    with_image_key
    extract_user
    get_user
    session_user_response
    me_response
    relationship_user_response
);

use constant USER_COLUMNS => qw(id username secret_key_hash display_name bio profile_image_id region account_type status);

sub valid_username ($value) {
    return defined $value && $value =~ /\A[a-z0-9_.-]{1,64}\z/;
}

sub valid_user_text ($value, $minimum, $maximum) {
    return false unless defined $value;
    my $length = length $value;
    return $length >= $minimum && $length <= $maximum;
}

sub valid_region ($region) {
    return defined $region && ($region eq 'america' || $region eq 'europe' || $region eq 'asia');
}

sub user_columns_as ($alias, $prefix) {
    return join ', ', map {qq{$alias.$_ AS "$prefix.$_"}} USER_COLUMNS;
}

sub with_image_key ($user) {
    $user->{image_key} = image_signing_key($user->{secret_key_hash});
    return $user;
}

# "author.id" のような列をまとめて取り出してユーザーにする
sub extract_user ($row, $prefix) {
    my %user = map { $_ => delete $row->{"$prefix.$_"} } USER_COLUMNS;
    return with_image_key(\%user);
}

sub get_user ($db, $user_id) {
    my $user = $db->query(
        'SELECT ' . join(', ', USER_COLUMNS) . q{ FROM users WHERE id = $1 AND status = 'active'},
        $user_id,
    )->hash or return undef;
    return with_image_key($user);
}

sub session_user_response ($user) {
    return {
        id          => $user->{id},
        username    => $user->{username},
        displayName => $user->{display_name},
        bio         => $user->{bio},
        region      => $user->{region},
        accountType => $user->{account_type},
        status      => $user->{status},
    };
}

sub profile_image_url ($user) {
    return undef unless defined $user->{profile_image_id};
    return signed_image_url($user, $user->{profile_image_id});
}

sub me_response ($user) {
    return {
        session_user_response($user)->%*,
        profileImageUrl => profile_image_url($user),
    };
}

sub relationship_user_response ($user) {
    return {
        id              => $user->{id},
        username        => $user->{username},
        displayName     => $user->{display_name},
        profileImageUrl => profile_image_url($user),
        bio             => $user->{bio},
        accountType     => $user->{account_type},
    };
}

1;
