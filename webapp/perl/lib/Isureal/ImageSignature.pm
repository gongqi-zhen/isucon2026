package Isureal::ImageSignature;
use v5.44;
use utf8;

use Exporter 'import';
use Digest::SHA qw(hmac_sha256);
use MIME::Base64 qw(encode_base64url);
use Mojo::Util qw(url_escape);

our @EXPORT_OK = qw(image_signing_key image_signature signed_image_url);

use constant IMAGE_URL_LIFETIME         => 24 * 60 * 60;
use constant IMAGE_SIGNATURE_ITERATIONS => 1000;

sub image_signing_key ($secret_key_hash) {
    die "invalid image signing key\n"
        unless defined $secret_key_hash && $secret_key_hash =~ /\A[0-9a-fA-F]{64}\z/;
    return pack 'H*', $secret_key_hash;
}

sub image_signature ($key, $image_id, $exp) {
    my $value = "$exp:$image_id";
    utf8::encode($value);
    $value = hmac_sha256($value, $key) for 1 .. IMAGE_SIGNATURE_ITERATIONS;
    return $value;
}

sub signed_image_url ($user, $image_id) {
    my $exp = time + IMAGE_URL_LIFETIME;
    my $sig = encode_base64url(image_signature($user->{image_key}, $image_id, $exp));
    return '/api/images/' . url_escape($image_id) . "?exp=$exp&sig=$sig";
}

1;
