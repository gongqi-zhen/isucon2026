package Isureal::APIError;
use v5.44;
use utf8;

use overload '""' => sub ($self, @) { $self->{code} }, fallback => 1;

sub new ($class, $status, $code) {
    return bless { status => $status, code => $code }, $class;
}

sub throw ($class, $status, $code) {
    die $class->new($status, $code);
}

sub status ($self) { $self->{status} }
sub code ($self)   { $self->{code} }

# リクエストの解釈に失敗したときのエラー
sub bad_request ($class) {
    $class->throw(400, 'code=400, message=Bad Request');
}

sub unsupported_media_type ($class) {
    $class->throw(415, 'code=415, message=Unsupported Media Type');
}

1;
