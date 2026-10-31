package Isureal::PostingWindow;
use v5.44;
use utf8;

use Exporter 'import';

our @EXPORT_OK = qw(get_posting_window);

sub get_posting_window ($db, $window_id) {
    return $db->query(
        q{SELECT id, region, local_date, starts_at, on_time_deadline, ended_at FROM posting_windows WHERE id = $1},
        $window_id,
    )->hash;
}

1;
