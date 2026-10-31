# frozen_string_literal: true

require 'openssl'

module Isureal
  class App
    GET_IMAGE_REQUEST = {
      image_id: { param: 'imageId' },
      exp: { query: 'exp' },
      sig: { query: 'sig' },
    }.freeze

    INT64_RANGE = (-(2**63)...(2**63))

    def parse_image_exp(value)
      return nil unless value.match?(/\A[+-]?\d+\z/)

      exp = value.to_i
      exp.to_s == value && INT64_RANGE.cover?(exp) ? exp : nil
    end

    def get_image_handler
      headers['cache-control'] = 'no-store'
      input = begin
        bind(GET_IMAGE_REQUEST)
      rescue HTTPError
        raise APIError.new(403, 'image_forbidden')
      end
      query, error = Bind.parse_query(request.query_string)
      if error || query.fetch('exp', []).length != 1 || query.fetch('sig', []).length != 1
        raise APIError.new(403, 'image_forbidden')
      end

      exp = parse_image_exp(input[:exp])
      raise APIError.new(403, 'image_forbidden') if exp.nil? || exp <= Time.now.to_i

      sig = ImageSignature.decode(input[:sig])
      raise APIError.new(403, 'image_forbidden') if sig.nil? || sig.bytesize != 32

      secret_key_hash = db.get_value(<<~SQL, input[:image_id])
        SELECT u.secret_key_hash
        FROM image_objects i JOIN users u ON u.id = i.owner_user_id
        WHERE i.id = $1 AND u.status = 'active'
      SQL
      raise APIError.new(403, 'image_forbidden') if secret_key_hash.nil?

      key = ImageSignature.signing_key(secret_key_hash)
      unless OpenSSL.fixed_length_secure_compare(sig, ImageSignature.signature(key, input[:image_id], exp))
        env['isureal.abuse'] = 'invalid_signature'
        raise APIError.new(403, 'image_forbidden')
      end

      # 削除状態を毎回確認する。即時反映も5秒以内の許容範囲に含まれる。
      object = db.get(<<~SQL, input[:image_id])
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
      raise APIError.new(403, 'image_forbidden') if object.nil?
      raise APIError.new(403, 'image_forbidden') if exp <= Time.now.to_i

      blob(200, object[:media_type], object[:data])
    end
  end
end
