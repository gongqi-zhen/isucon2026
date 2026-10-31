# frozen_string_literal: true

module Isureal
  class App
    SEARCH_USERS_REQUEST = {
      q: { query: 'q' },
      before_username: { query: 'beforeUsername' },
      before_id: { query: 'beforeId' },
    }.freeze

    GET_USER_REQUEST = {
      user_id: { param: 'userId' },
    }.freeze

    def normalize_search_text(value)
      value.unicode_normalize(:nfc).downcase(:fold).unicode_normalize(:nfc)
    end

    def search_users_handler
      input = bind(SEARCH_USERS_REQUEST)
      raise APIError.new(400, 'invalid_relationship') unless valid_user_text?(input[:q], 1, 100)
      raise APIError.new(400, 'invalid_pagination') if input[:before_username].empty? != input[:before_id].empty?

      viewer = current_session.user_id
      users = db.select("SELECT #{USER_COLUMNS.join(', ')} FROM users WHERE status='active' AND id<>$1 AND ($2='' OR (username,id)>($2,$3)) ORDER BY username, id",
                        viewer, input[:before_username], input[:before_id])
      blocked = get_blocked_user_ids(db, viewer)
      query = normalize_search_text(input[:q])
      items = []
      users.each do |row|
        user = User.from_row(row)
        next if !normalize_search_text(user.username).include?(query) && !normalize_search_text(user.display_name).include?(query)
        next if blocked[user.id]

        user.set_image_key
        user.bio = '' if user.account_type != 'certified_public'
        items << relationship_user_response(user)
        break if items.length == 51
      end
      has_more = items.length > 50
      items = items.first(50) if has_more
      json_response(200, { items:, hasMore: has_more })
    end

    def get_user_handler
      input = bind(GET_USER_REQUEST)
      viewer = current_session.user_id
      user = get_user(db, input[:user_id])
      raise APIError.new(404, 'relationship_not_found') if user.nil?
      raise APIError.new(404, 'relationship_not_found') if blocked?(db, viewer, user.id)

      friends = get_friend_ids(db, viewer)
      user.bio = '' if user.account_type != 'certified_public' && !friends.include?(user.id)
      json_response(200, relationship_user_response(user))
    end

    def relationship_user_response(user)
      {
        id: user.id, username: user.username, displayName: user.display_name,
        profileImageUrl: profile_image_url(user), bio: user.bio, accountType: user.account_type,
      }
    end
  end
end
