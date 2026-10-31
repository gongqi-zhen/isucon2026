package Isureal::Controller::Image;
use v5.44;
use utf8;
use Mojo::Base 'Isureal::Controller';

use MIME::Base64 qw(decode_base64url encode_base64url);
use Isureal::APIError;
use Isureal::ImageSignature qw(image_signing_key image_signature);
use Isureal::Util qw(secure_compare);

sub forbidden () {
    Isureal::APIError->throw(403, 'image_forbidden');
}

sub show ($c) {
    $c->res->headers->cache_control('no-store');
    my $image_id = $c->param('imageId');
    my $query    = $c->req->query_params;
    my $exps     = $query->every_param('exp');
    my $sigs     = $query->every_param('sig');
    forbidden() unless @$exps == 1 && @$sigs == 1;
    my ($exp, $sig) = ($exps->[0], $sigs->[0]);

    # int64 の範囲の正規化された 10 進数だけを受け付ける（負の値は期限切れとして扱う）
    forbidden() unless $exp =~ /\A(?:0|[1-9][0-9]{0,18})\z/ && (length $exp < 19 || $exp le '9223372036854775807');
    forbidden() if $exp <= time;
    forbidden() unless $sig =~ /\A[A-Za-z0-9_-]{43}\z/;
    my $signature = decode_base64url($sig);
    forbidden() unless length $signature == 32 && encode_base64url($signature) eq $sig;

    my $db  = $c->db;
    my $row = $db->query(<<~'SQL', $image_id)->array or forbidden();
           SELECT u.secret_key_hash
           FROM image_objects i JOIN users u ON u.id = i.owner_user_id
           WHERE i.id = $1 AND u.status = 'active'
        SQL
    my $key = image_signing_key($row->[0]);
    unless (secure_compare($signature, image_signature($key, $image_id, $exp))) {
        $c->stash('isureal.abuse' => 'invalid_signature');
        forbidden();
    }

    # 削除状態を毎回確認する。即時反映も5秒以内の許容範囲に含まれる。
    my $object = $db->query(<<~'SQL', $image_id)->hash or forbidden();
           SELECT i.data, i.media_type
           FROM image_objects i JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'
           WHERE i.id = $1 AND (
             u.profile_image_id = i.id
             OR EXISTS (
               SELECT 1 FROM posts p
               WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL
             )
             OR EXISTS (
               SELECT 1 FROM realmojis r
               JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
               JOIN users author ON author.id = p.author_id AND author.status = 'active'
               WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
             )
           )
        SQL
    forbidden() if $exp <= time;
    $c->res->headers->content_type($object->{media_type});
    $c->render(data => $object->{data});
}

1;
