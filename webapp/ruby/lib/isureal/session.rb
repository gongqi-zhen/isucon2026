# frozen_string_literal: true

require 'digest'
require 'securerandom'

module Isureal
  Session = Struct.new(:region, :token, :user_id, :token_hash, :created_at, keyword_init: true)

  class App
    def current_session = env['isureal.session']

    def session_from_header
      auth_header = env['HTTP_AUTHORIZATION']
      return nil if auth_header.nil? || auth_header.empty?

      unless auth_header.start_with?('Bearer ')
        Log.warn('Invalid authorization header format(not started with Bearer)')
        raise APIError.new(401, 'invalid_session')
      end

      token = auth_header.delete_prefix('Bearer ')
      token_hash = Digest::SHA256.hexdigest(token)
      row = db.get(<<~SQL, token_hash)
        SELECT s.user_id, s.token_hash, s.created_at, u.region
        FROM login_sessions s JOIN users u ON u.id = s.user_id
        WHERE s.token_hash = $1
          AND s.active = true AND u.status = 'active'
      SQL
      row && Session.new(**row)
    end

    def session_middleware(auth_required)
      session = session_from_header
      raise APIError.new(401, 'invalid_session') if session.nil? && auth_required

      env['isureal.session'] = session
      yield
    end

    def invalidate_login_session(user_id, token_hash)
      db.transaction do |tx|
        tx.get_value!('SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE', user_id)
        tx.exec(<<~SQL, user_id, token_hash)
          UPDATE login_sessions
          SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
          WHERE user_id = $1 AND active = true AND token_hash = $2
        SQL
      end
    end

    # 2026-09-03: リファクタリング後にこの関数の利用がないようです。消すのはいかがでしょうか
    def create_login_session(user_id)
      db.transaction { |tx| create_login_session_tx(tx, user_id) }
    end

    def create_login_session_tx(tx, user_id)
      random_token = SecureRandom.uuid
      token_hash = Digest::SHA256.hexdigest(random_token)
      tx.get_value!("SELECT id FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE", user_id)
      tx.exec(<<~SQL, user_id)
        UPDATE login_sessions
        SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
        WHERE user_id = $1 AND active = true
      SQL
      session_id = SecureRandom.uuid
      tx.exec(<<~SQL, session_id, user_id, token_hash)
        INSERT INTO login_sessions (id, user_id, token_hash)
        VALUES ($1, $2, $3)
      SQL
      session = get_login_session(tx, session_id)
      session.token = random_token
      session
    end

    def get_login_session(db, session_id)
      Session.new(**db.get!('SELECT user_id, token_hash, created_at FROM login_sessions WHERE id = $1', session_id))
    end
  end
end
