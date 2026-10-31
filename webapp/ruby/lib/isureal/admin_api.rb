# frozen_string_literal: true

require 'digest'

module Isureal
  class App
    ADMIN_LOGIN_REQUEST = {
      username: { json: 'username' },
      password: { json: 'password' },
    }.freeze

    def admin_login_handler
      input = bind(ADMIN_LOGIN_REQUEST)
      raise APIError.new(401, 'invalid_credentials') unless USERNAME_PATTERN.match?(input[:username])

      administrator = db.get('SELECT id, password_hash FROM administrators WHERE username = $1', input[:username])
      raise APIError.new(401, 'invalid_credentials') if administrator.nil?
      raise APIError.new(401, 'invalid_credentials') if administrator[:password_hash] != Digest::SHA256.hexdigest(input[:password])

      session = create_admin_session(administrator[:id])
      json_response(200, { token: session.token, administratorId: session.administrator_id })
    end

    def admin_logout_handler
      session = current_admin_session
      raise APIError.new(401, 'invalid_administrator_session') if session.nil?

      invalidate_admin_session(session.administrator_id, session.token_hash)
      no_content(204)
    end
  end
end
