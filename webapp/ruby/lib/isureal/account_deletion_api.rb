# frozen_string_literal: true

module Isureal
  class App
    def delete_account_handler
      user_id = current_session.user_id
      db.transaction do |tx|
        locked_id = tx.get_value("SELECT id FROM users WHERE id=$1 AND status='active' FOR NO KEY UPDATE", user_id)
        raise APIError.new(404, 'account_not_found') if locked_id.nil?

        tx.exec("UPDATE users SET status='deleted', deleted_at=clock_timestamp(), updated_at=clock_timestamp() WHERE id=$1", user_id)
        tx.exec("UPDATE login_sessions SET active=false, invalidated_at=clock_timestamp(), reason='account_deletion' WHERE user_id=$1 AND active=true", user_id)
      end
      no_content(204)
    end
  end
end
