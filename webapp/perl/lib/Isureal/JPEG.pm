package Isureal::JPEG;
use v5.44;
use utf8;

use Exporter 'import';
use Imager;

our @EXPORT_OK = qw(decode_jpeg);

sub decode_jpeg ($data) {
    my $image = Imager->new(data => $data, type => 'jpeg');
    return undef if !$image || Imager::i_errors();
    return { width => $image->getwidth, height => $image->getheight };
}

1;
