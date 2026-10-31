package Isureal::RealMoji;
use v5.44;
use utf8;

use Exporter 'import';
use Isureal::ImageSignature qw(signed_image_url);
use Isureal::User qw(extract_user user_columns_as relationship_user_response);
use Isureal::Util qw(rfc3339);

our @EXPORT_OK = qw(get_real_moji select_current_real_mojis real_moji_response);

sub get_real_moji ($db, $id) {
    return $db->query(
        q{SELECT id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at, deleted_at FROM realmojis WHERE id=$1},
        $id,
    )->hash;
}

sub select_current_real_mojis ($db, $conditions, @args) {
    my $reactions = $db->query(<<~"SQL", @args)->hashes;
          SELECT r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at,
                 ${\user_columns_as('u', 'reactor')}
          FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
          WHERE r.replaced_at IS NULL AND r.deleted_at IS NULL $conditions
        SQL
    return $reactions->map(sub ($row) { +{ real_moji => $row, reactor => extract_user($row, 'reactor') } });
}

sub real_moji_response ($reaction, $user) {
    return {
        id           => $reaction->{id},
        reactor      => relationship_user_response($user),
        reactionType => $reaction->{reaction_type},
        imageUrl     => signed_image_url($user, $reaction->{image_id}),
        createdAt    => rfc3339($reaction->{created_at}),
    };
}

1;
