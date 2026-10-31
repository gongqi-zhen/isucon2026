# frozen_string_literal: true

module Isureal
  USERNAME_PATTERN = /\A[a-z0-9_.-]{1,64}\z/

  USER_COLUMNS = %w[id username secret_key_hash display_name bio profile_image_id region account_type status].freeze
  USER_FIELDS = USER_COLUMNS.map(&:to_sym).freeze

  User = Struct.new(*USER_FIELDS, :image_key, keyword_init: true) do
    def self.from_row(row, prefix = nil)
      return new(**row.slice(*USER_FIELDS)) if prefix.nil?

      new(**USER_FIELDS.to_h { |field| [field, row[:"#{prefix}.#{field}"]] })
    end

    def set_image_key
      self.image_key = ImageSignature.signing_key(secret_key_hash)
      self
    end
  end

  module UserValidation
    module_function

    def valid_user_text?(value, minimum, maximum)
      value.is_a?(String) && value.valid_encoding? && value.length.between?(minimum, maximum)
    end

    def valid_region?(region)
      %w[america europe asia].include?(region)
    end
  end

  class App
    include UserValidation

    def user_columns_as(table_alias, prefix)
      USER_COLUMNS.map { |column| %(#{table_alias}.#{column} AS "#{prefix}.#{column}") }.join(', ')
    end

    def get_user(db, user_id)
      row = db.get("SELECT #{USER_COLUMNS.join(', ')} FROM users WHERE id = $1 AND status = 'active'", user_id)
      row && User.from_row(row).set_image_key
    end
  end
end
