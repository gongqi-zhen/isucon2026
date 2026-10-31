# frozen_string_literal: true

module Isureal
  REALMOJI_FIELDS = %i[id post_id reactor_id image_id reaction_type created_at replaced_at deleted_at].freeze

  RealMoji = Struct.new(*REALMOJI_FIELDS, :reactor, keyword_init: true) do
    def self.from_row(row)
      new(**row.slice(*REALMOJI_FIELDS))
    end
  end

  class App
    def get_real_moji(db, id)
      row = db.get('SELECT id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at, deleted_at FROM realmojis WHERE id=$1', id)
      row && RealMoji.from_row(row)
    end

    def select_current_realmojis(db, conditions, *args)
      rows = db.select(<<~SQL, *args)
        SELECT r.id, r.post_id, r.reactor_id, r.image_id, r.reaction_type, r.created_at, r.replaced_at, r.deleted_at,
               #{user_columns_as('u', 'reactor')}
        FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
        WHERE r.replaced_at IS NULL AND r.deleted_at IS NULL #{conditions}
      SQL
      rows.map { |row| RealMoji.from_row(row).tap { |reaction| reaction.reactor = User.from_row(row, 'reactor').set_image_key } }
    end
  end
end
