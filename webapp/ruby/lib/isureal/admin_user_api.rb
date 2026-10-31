# frozen_string_literal: true

module Isureal
  class App
    CERTIFY_USER_REQUEST = {
      user_id: { param: 'userId' },
    }.freeze

    def certify_user_handler
      input = bind(CERTIFY_USER_REQUEST)
      count = db.exec("UPDATE users SET account_type='certified_public', updated_at=clock_timestamp() WHERE id=$1 AND status='active' AND account_type='regular'", input[:user_id])
      raise APIError.new(409, 'certification_conflict') if count.zero?

      no_content(204)
    end

    # 2026-08-26: 認定されたユーザーを戻すことは禁止する
    def delete_user_certification_handler
      raise APIError.new(409, 'certification_irreversible')
    end
  end
end
