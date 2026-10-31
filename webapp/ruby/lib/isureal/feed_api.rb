# frozen_string_literal: true

module Isureal
  class App
    FEEDS = %w[friends friends-of-friends public following].freeze

    GET_FEED_REQUEST = {
      feed: { param: 'feed' },
      before_score: { query: 'beforeScore', type: :int },
      before_posted_at: { query: 'beforePostedAt', type: :time },
      before_id: { query: 'beforeId' },
    }.freeze

    def get_feed_handler
      input = bind(GET_FEED_REQUEST)
      raise APIError.new(400, 'invalid_post') unless FEEDS.include?(input[:feed])
      raise APIError.new(400, 'invalid_pagination') if input[:before_posted_at].nil? != input[:before_id].empty?

      ranked = input[:feed] == 'friends-of-friends'
      raise APIError.new(400, 'invalid_pagination') if ranked && input[:before_score].nil? != input[:before_id].empty?

      viewer = load_viewer_context(db, current_session.user_id)
      return json_response(200, { items: [], hasMore: false }) if viewer.nil?

      following = {}
      if input[:feed] == 'following'
        db.select_values('SELECT followee_id FROM follows WHERE follower_id = $1', viewer.id).each { |id| following[id] = true }
      end
      before_posted_at, before_id = ranked ? [nil, ''] : input.values_at(:before_posted_at, :before_id)
      posts = select_posts_with_author(db, <<~SQL, viewer.id, before_posted_at, before_id)
        AND p.author_id <> $1 AND w.ended_at IS NULL
        AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
        ORDER BY p.posted_at DESC, p.id DESC
      SQL
      candidates = posts.select { |post| viewer.can_view?(post) && matches_feed?(viewer, following, post, input[:feed]) }
      scores = {}
      if ranked
        scores = recommendation_scores(db, viewer, candidates)
        candidates = candidates.sort_by.with_index { |post, index| [-scores[post.id], index] }
      end
      items = []
      candidates.each do |post|
        score = scores[post.id]
        next if ranked && !input[:before_score].nil? && !ranked_before?(score, post, *input.values_at(:before_score, :before_posted_at, :before_id))

        item = post_response(post, post.author)
        item[:score] = score if ranked
        items << item
        break if items.length == 51
      end
      has_more = items.length > 50
      items = items.first(50) if has_more
      json_response(200, { items:, hasMore: has_more })
    end

    def ranked_before?(score, post, before_score, before_posted_at, before_id)
      return score < before_score if score != before_score
      return post.posted_at < before_posted_at if post.posted_at != before_posted_at

      post.id < before_id
    end

    def recommendation_scores(db, viewer, posts)
      return {} if posts.empty?

      author_ids = posts.map(&:author_id)
      post_ids = posts.map(&:id)

      friendships = db.select(
        "SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN (#{in_placeholders(1, author_ids.length)}) OR user_high_id IN (#{in_placeholders(author_ids.length + 1, author_ids.length)})",
        *author_ids, *author_ids
      )
      connections = Hash.new(0)
      friendships.each do |friendship|
        connections[friendship[:user_low_id]] += viewer.mutual_friends[friendship[:user_high_id]]
        connections[friendship[:user_high_id]] += viewer.mutual_friends[friendship[:user_low_id]]
      end

      engagements = db.select(<<~SQL, *post_ids, *post_ids)
        SELECT r.post_id, r.reactor_id AS user_id FROM realmojis r
        JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
        WHERE r.post_id IN (#{in_placeholders(1, post_ids.length)}) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
        UNION
        SELECT c.post_id, c.author_id AS user_id FROM comments c
        JOIN users u ON u.id = c.author_id AND u.status = 'active'
        WHERE c.post_id IN (#{in_placeholders(post_ids.length + 1, post_ids.length)}) AND c.deleted_at IS NULL
      SQL
      friend_engagements = Hash.new(0)
      engagements.each do |engagement|
        friend_engagements[engagement[:post_id]] += 1 if viewer.friends[engagement[:user_id]]
      end

      posts.to_h do |post|
        [post.id, (10 * friend_engagements[post.id]) + (3 * viewer.mutual_friends[post.author_id]) + [connections[post.author_id], 30].min]
      end
    end

    def matches_feed?(viewer, following, post, feed)
      case feed
      when 'public' then post.visibility == 'public'
      when 'following' then following[post.author_id]
      when 'friends' then viewer.friends[post.author_id]
      when 'friends-of-friends' then !viewer.friends[post.author_id] && viewer.friends_of_friends[post.author_id]
      else false
      end
    end
  end
end
