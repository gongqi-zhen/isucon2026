# frozen_string_literal: true

module Isureal
  class App
    GET_RELATIONSHIPS_REQUEST = {
      kind: { param: 'kind' },
      before_updated_at: { query: 'beforeUpdatedAt', type: :time },
      before_id: { query: 'beforeId' },
    }.freeze

    RELATIONSHIP_QUERIES = {
      'friends' => 'SELECT id, user_low_id AS first_id, user_high_id AS second_id, updated_at FROM friendships WHERE user_low_id=$1 OR user_high_id=$1',
      'following' => 'SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE follower_id=$1',
      'followers' => 'SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE followee_id=$1',
      'incoming-requests' => "SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE recipient_id=$1 AND status='pending'",
      'outgoing-requests' => "SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE requester_id=$1 AND status='pending'",
    }.freeze

    def get_relationships_handler
      input = bind(GET_RELATIONSHIPS_REQUEST)
      raise APIError.new(400, 'invalid_pagination') if input[:before_updated_at].nil? != input[:before_id].empty?

      query = RELATIONSHIP_QUERIES[input[:kind]]
      raise APIError.new(400, 'invalid_relationship') if query.nil?

      actor = current_session.user_id
      records = db.select(<<~SQL, actor, input[:before_updated_at], input[:before_id])
        SELECT relations.id, relations.updated_at, #{user_columns_as('u', 'user')}
        FROM (#{query}) relations
        JOIN users u ON u.status = 'active'
                    AND u.id = CASE WHEN relations.first_id = $1 THEN relations.second_id ELSE relations.first_id END
        WHERE ($2::timestamptz IS NULL OR (relations.updated_at, relations.id) < ($2, $3))
        ORDER BY relations.updated_at DESC, relations.id DESC LIMIT 51
      SQL
      items = records.map do |record|
        user = User.from_row(record, 'user').set_image_key
        user.bio = '' if user.account_type != 'certified_public' && input[:kind] != 'friends'
        status = %w[incoming-requests outgoing-requests].include?(input[:kind]) ? 'pending' : input[:kind]
        {
          relationId: record[:id], user: relationship_user_response(user),
          status:, updatedAt: Response.format_time(record[:updated_at]),
        }
      end
      has_more = items.length > 50
      items = items.first(50) if has_more
      json_response(200, { items:, hasMore: has_more })
    end
  end
end
