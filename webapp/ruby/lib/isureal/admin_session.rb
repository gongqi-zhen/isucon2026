# frozen_string_literal: true

require 'digest'
require 'securerandom'

module Isureal
  AdminSession = Struct.new(:token, :administrator_id, :token_hash, :created_at, keyword_init: true)

  class App
    def current_admin_session = env['isureal.admin_session']

    def admin_session_from_header
      auth_header = env['HTTP_AUTHORIZATION']
      return nil if auth_header.nil? || auth_header.empty?
      raise APIError.new(401, 'invalid_administrator_session') unless auth_header.start_with?('Bearer ')

      token = auth_header.delete_prefix('Bearer ')
      row = db.get(<<~SQL, Digest::SHA256.hexdigest(token))
        SELECT administrator_id, token_hash, created_at
        FROM administrator_sessions
        WHERE token_hash = $1 AND active = true
      SQL
      row && AdminSession.new(**row)
    end

    def admin_session_middleware(auth_required)
      session = admin_session_from_header
      raise APIError.new(401, 'invalid_administrator_session') if session.nil? && auth_required

      env['isureal.admin_session'] = session
      yield
    end

    def create_admin_session(administrator_id)
      db.transaction do |tx|
        session_id = SecureRandom.uuid
        token = SecureRandom.uuid
        tx.exec(<<~SQL, session_id, administrator_id, Digest::SHA256.hexdigest(token))
          INSERT INTO administrator_sessions (id, administrator_id, token_hash)
          VALUES ($1, $2, $3)
        SQL
        session = get_admin_session(tx, session_id)
        session.token = token
        session
      end
    end

    def get_admin_session(db, session_id)
      AdminSession.new(**db.get!('SELECT administrator_id, token_hash, created_at FROM administrator_sessions WHERE id = $1', session_id))
    end

    def invalidate_admin_session(administrator_id, token_hash)
      db.exec(<<~SQL, administrator_id, token_hash)
        UPDATE administrator_sessions
        SET active = false, invalidated_at = clock_timestamp()
        WHERE administrator_id = $1 AND token_hash = $2 AND active = true
      SQL
    end
  end
end
