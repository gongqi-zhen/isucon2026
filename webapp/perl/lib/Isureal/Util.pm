package Isureal::Util;
use v5.44;
use utf8;

use Exporter 'import';
use Crypt::URandom ();
use Digest::SHA ();
use Time::Moment;

our @EXPORT_OK = qw(
    NO_ROWS
    new_uuid
    sha256_hex
    secure_compare
    min_max
    placeholders
    rfc3339
    moment
    now_utc
    parse_rfc3339
    parse_date
);

# 行が見つからなかったときのエラー
use constant NO_ROWS => "sql: no rows in result set\n";

sub new_uuid () {
    my @bytes = unpack 'C16', Crypt::URandom::urandom(16);
    $bytes[6] = ($bytes[6] & 0x0f) | 0x40;
    $bytes[8] = ($bytes[8] & 0x3f) | 0x80;
    return sprintf '%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x', @bytes;
}

sub sha256_hex ($value) {
    utf8::encode($value) if utf8::is_utf8($value);
    return Digest::SHA::sha256_hex($value);
}

sub secure_compare ($x, $y) {
    return false unless length $x == length $y;
    my $diff = 0;
    $diff |= ord(substr $x, $_, 1) ^ ord(substr $y, $_, 1) for 0 .. length($x) - 1;
    return $diff == 0;
}

sub min_max ($x, $y) {
    return $x lt $y ? ($x, $y) : ($y, $x);
}

# $1, $2, ... のプレースホルダを $count 個並べる
sub placeholders ($count, $offset = 0) {
    return join ', ', map { '$' . ($offset + $_) } 1 .. $count;
}

# PostgreSQL の timestamptz（TimeZone=UTC）を RFC 3339 の文字列にする
sub rfc3339 ($timestamp) {
    return undef unless defined $timestamp;
    $timestamp =~ /\A(\d{4}-\d\d-\d\d) (\d\d:\d\d:\d\d(?:\.\d+)?)\+00\z/
        or die "unexpected timestamp format: $timestamp\n";
    return "$1T$2Z";
}

sub moment ($timestamp) {
    return undef unless defined $timestamp;
    return Time::Moment->from_string(rfc3339($timestamp));
}

sub now_utc () {
    return Time::Moment->now_utc->with_precision(6);
}

# クエリパラメータの時刻を解釈する。解釈できなければ undef
sub parse_rfc3339 ($value) {
    return undef unless defined $value
        && $value =~ /\A\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)\z/;
    my $moment = eval { Time::Moment->from_string($value) } or return undef;
    return $moment->with_offset_same_instant(0)->with_precision(6);
}

# YYYY-MM-DD を解釈する。解釈できなければ undef
sub parse_date ($value) {
    return undef unless defined $value && $value =~ /\A\d{4}-\d\d-\d\d\z/;
    return eval { Time::Moment->from_string("${value}T00:00:00Z") };
}

1;
