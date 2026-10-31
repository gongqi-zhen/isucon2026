# frozen_string_literal: true

require 'digest'
require 'securerandom'

module Isureal
  class App
    REGISTER_REQUEST = {
      username: { json: 'username' },
      display_name: { json: 'displayName' },
      region: { json: 'region' },
    }.freeze

    LOGIN_REQUEST = {
      username: { json: 'username' },
      secret_key: { json: 'secretKey' },
    }.freeze

    UPDATE_ME_REQUEST = {
      display_name: { json: 'displayName', type: :string_pointer },
      bio: { json: 'bio', type: :string_pointer },
      region: { json: 'region', type: :string_pointer },
    }.freeze

    def session_user_response(user)
      {
        id: user.id, username: user.username, displayName: user.display_name,
        bio: user.bio, region: user.region, accountType: user.account_type, status: user.status,
      }
    end

    def get_me_response(user)
      session_user_response(user).merge(profileImageUrl: profile_image_url(user))
    end

    def profile_image_url(user)
      user.profile_image_id && ImageSignature.signed_url(user, user.profile_image_id)
    end

    def register_handler
      input = bind(REGISTER_REQUEST)
      env['isureal.username'] = input[:username]
      unless USERNAME_PATTERN.match?(input[:username]) && valid_user_text?(input[:display_name], 1, 100) && valid_region?(input[:region])
        raise APIError.new(400, 'invalid_registration')
      end

      secret_key = SecureRandom.uuid
      user = db.transaction do |tx|
        # ユーザー名が一意である必要があります。
        tx.exec('LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE')
        if tx.get_value!('SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)', input[:username])
          raise APIError.new(409, 'username_taken')
        end

        user_id = SecureRandom.uuid
        tx.exec('INSERT INTO users (id, username, secret_key_hash, display_name, region) VALUES ($1, $2, $3, $4, $5)',
                user_id, input[:username], Digest::SHA256.hexdigest(secret_key), input[:display_name], input[:region])
        get_user(tx, user_id) or raise NoRowsError
      end
      json_response(201, { secretKey: secret_key, user: session_user_response(user) })
    end

    def login_handler
      input = bind(LOGIN_REQUEST)
      env['isureal.username'] = input[:username]
      raise APIError.new(401, 'invalid_credentials') unless USERNAME_PATTERN.match?(input[:username])

      user, session = db.transaction do |tx|
        row = tx.get('SELECT id, username, secret_key_hash, display_name, bio, profile_image_id, region, account_type, status FROM users WHERE username = $1 FOR NO KEY UPDATE', input[:username])
        raise APIError.new(401, 'invalid_credentials') if row.nil?

        user = User.from_row(row)
        if user.status != 'active' || user.secret_key_hash != Digest::SHA256.hexdigest(input[:secret_key])
          raise APIError.new(401, 'invalid_credentials')
        end

        [user, create_login_session_tx(tx, user.id)]
      end
      json_response(200, { token: session.token, user: session_user_response(user) })
    end

    def logout_handler
      session = current_session
      raise APIError.new(401, 'invalid_session') if session.nil?

      invalidate_login_session(session.user_id, session.token_hash)
      no_content(204)
    end

    def get_me_handler
      session = current_session
      raise APIError.new(401, 'invalid_session') if session.nil?

      user = get_user(db, session.user_id)
      raise APIError.new(404, 'account_not_found') if user.nil?

      json_response(200, get_me_response(user))
    end

    def update_me_handler
      session = current_session
      raise APIError.new(401, 'invalid_session') if session.nil?

      input = bind(UPDATE_ME_REQUEST)
      if (input[:display_name].nil? && input[:bio].nil? && input[:region].nil?) ||
         (!input[:display_name].nil? && !valid_user_text?(input[:display_name], 1, 100)) ||
         (!input[:bio].nil? && !valid_user_text?(input[:bio], 0, 500)) ||
         (!input[:region].nil? && !valid_region?(input[:region]))
        raise APIError.new(400, 'invalid_profile')
      end

      user = db.transaction do |tx|
        unless input[:display_name].nil?
          tx.exec('UPDATE users SET display_name = $2, updated_at = clock_timestamp() WHERE id = $1', session.user_id, input[:display_name])
        end
        unless input[:bio].nil?
          tx.exec('UPDATE users SET bio = $2, updated_at = clock_timestamp() WHERE id = $1', session.user_id, input[:bio])
        end
        unless input[:region].nil?
          tx.exec('UPDATE users SET region = $2, updated_at = clock_timestamp() WHERE id = $1', session.user_id, input[:region])
        end
        user = get_user(tx, session.user_id)
        raise APIError.new(404, 'account_not_found') if user.nil?

        user
      end
      json_response(200, get_me_response(user))
    end
  end
end
