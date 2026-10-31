package Isureal::Controller;
use v5.44;
use utf8;
use Mojo::Base 'Mojolicious::Controller';

use builtin qw(created_as_number created_as_string);
no warnings qw(experimental::builtin);
use Mojo::JSON qw(decode_json);
use Scalar::Util qw(looks_like_number);
use Isureal::APIError;
use Isureal::Util qw(parse_rfc3339);

sub current_session ($c)       { $c->stash->{'isureal.session'} }
sub current_admin_session ($c) { $c->stash->{'isureal.admin_session'} }

sub render_error ($c, $error) {
    my ($status, $message) = (500, "$error");
    if ($error isa Isureal::APIError) {
        ($status, $message) = ($error->status, $error->code);
    }
    chomp $message;
    $c->app->log->error("request failed: $message") if $status >= 500;
    $c->render(status => $status, json => { error => $message });
}

sub no_content ($c) {
    $c->rendered(204);
}

# JSON のリクエストボディを %spec に従って取り出す。
# spec の型は string（省略時は ''）、?string（省略時は undef）、?int（省略時は undef）
sub json_body ($c, %spec) {
    my $req  = $c->req;
    my $body = $req->body;
    my %input = map { $_ => ($spec{$_} eq 'string' ? '' : undef) } keys %spec;
    return \%input if $body eq '';

    my ($media_type) = split /;/, $req->headers->content_type // '';
    Isureal::APIError->unsupported_media_type unless trim($media_type) eq 'application/json';

    my $data = eval { decode_json($body) };
    Isureal::APIError->bad_request if $@ || (defined $data && ref $data ne 'HASH');
    for my $key (keys %spec) {
        my $value = $data->{$key};
        next unless defined $value;
        if ($spec{$key} eq '?int') {
            Isureal::APIError->bad_request
                unless !ref $value && created_as_number($value) && $value =~ /\A-?\d+\z/;
        }
        else {
            Isureal::APIError->bad_request unless !ref $value && created_as_string($value);
        }
        $input{$key} = $value;
    }
    return \%input;
}

# multipart/form-data のリクエストボディから値を取り出す。
# 返り値は { fields => { name => value }, uploads => { name => Mojo::Upload } }。
# 同じ名前の値が複数あれば最初の値を使う
sub form_body ($c) {
    my $req  = $c->req;
    my $form = { fields => {}, uploads => {} };
    return $form if $req->body_size == 0;

    my ($media_type) = split /;/, $req->headers->content_type // '';
    $media_type = trim($media_type);
    if ($media_type eq 'multipart/form-data' || $media_type eq 'application/x-www-form-urlencoded') {
        Isureal::APIError->bad_request if $req->error;
        for my ($name, $value) ($req->body_params->pairs->@*) {
            $form->{fields}{$name} //= $value;
        }
        $form->{uploads}{$_->name} //= $_ for $req->uploads->@*;
        return $form;
    }
    if ($media_type eq 'application/json') {
        my $data = eval { decode_json($req->body) };
        Isureal::APIError->bad_request if $@ || (defined $data && ref $data ne 'HASH');
        return $form;
    }
    Isureal::APIError->unsupported_media_type;
}

# フォームの数値を解釈する。送られていなければ undef
sub form_number ($c, $value) {
    return undef unless defined $value;
    return 0 if $value eq '';
    Isureal::APIError->bad_request unless looks_like_number($value) && $value !~ /\A\s|\s\z/;
    return 0 + $value;
}

# 時刻のクエリパラメータを取り出す。送られていなければ undef
sub query_time ($c, $name) {
    my $value = first_param($c->req->query_params, $name);
    return undef unless defined $value;
    return parse_rfc3339($value) // Isureal::APIError->bad_request;
}

sub query_int ($c, $name) {
    my $value = first_param($c->req->query_params, $name);
    return undef unless defined $value;
    return 0 if $value eq '';
    my ($sign, $digits) = $value =~ /\A([+-]?)0*([0-9]+)\z/ or Isureal::APIError->bad_request;
    Isureal::APIError->bad_request
        if length $digits > 19 || (length $digits == 19 && $digits gt ($sign eq '-' ? '9223372036854775808' : '9223372036854775807'));
    return $sign eq '-' ? -(0 + $digits) : 0 + $digits;
}

sub query_string ($c, $name) {
    return first_param($c->req->query_params, $name) // '';
}

# 同じ名前の値が複数あれば最初の値を使う
sub first_param ($params, $name) {
    return $params->every_param($name)->[0];
}

1;
