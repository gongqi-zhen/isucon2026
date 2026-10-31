# frozen_string_literal: true

module Isureal
  POST_FIELDS = %i[id author_id posting_window_id image_id caption visibility location_kind latitude longitude locality
                   posted_at on_time_deadline window_ended_at].freeze

  Post = Struct.new(*POST_FIELDS, :author, keyword_init: true) do
    def self.from_row(row)
      new(**row.slice(*POST_FIELDS))
    end

    def location
      { kind: location_kind, latitude:, longitude:, locality: }.compact
    end
  end

  module PostLocation
    module_function

    def valid?(kind:, latitude:, longitude:, locality:)
      case kind
      when 'none'
        latitude.nil? && longitude.nil? && locality.nil?
      when 'exact'
        !latitude.nil? && !longitude.nil? && locality.nil? &&
          latitude >= -90 && latitude <= 90 && longitude >= -180 && longitude <= 180
      when 'approximate'
        latitude.nil? && longitude.nil? && !locality.nil? && UserValidation.valid_user_text?(locality, 1, 100)
      else
        false
      end
    end
  end

  ViewerContext = Struct.new(:user, :has_posted_today, :friends, :friends_of_friends, :mutual_friends, :blocked,
                             keyword_init: true) do
    def id = user.id

    def region = user.region

    def can_view?(post)
      return true if post.author_id == id
      return false if !post.window_ended_at.nil? || !has_posted_today || blocked[post.author_id]

      case post.visibility
      when 'public' then true
      when 'friends' then friends[post.author_id]
      when 'friends_of_friends' then friends[post.author_id] || friends_of_friends[post.author_id]
      else false
      end
    end
  end

  class App
    POST_COLUMNS = <<~SQL.chomp
      p.id, p.author_id, p.posting_window_id, p.image_id, p.caption, p.visibility,
        p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at,
        w.on_time_deadline, w.ended_at AS window_ended_at
    SQL

    def in_placeholders(start, count)
      (start...(start + count)).map { |i| "$#{i}" }.join(', ')
    end

    def get_post(db, post_id)
      row = db.get(<<~SQL, post_id)
        SELECT #{POST_COLUMNS}
        FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
        WHERE p.id = $1 AND p.deleted_at IS NULL
      SQL
      row && Post.from_row(row)
    end

    def select_posts_with_author(db, conditions, *args)
      rows = db.select(<<~SQL, *args)
        SELECT #{POST_COLUMNS}, #{user_columns_as('u', 'author')}
        FROM posts p
        JOIN posting_windows w ON w.id = p.posting_window_id
        JOIN users u ON u.id = p.author_id AND u.status = 'active'
        WHERE p.deleted_at IS NULL #{conditions}
      SQL
      rows.map { |row| Post.from_row(row).tap { |post| post.author = User.from_row(row, 'author').set_image_key } }
    end

    def get_post_with_author(db, post_id)
      select_posts_with_author(db, 'AND p.id = $1', post_id).first
    end

    def load_viewer_context(db, viewer_id)
      user = get_user(db, viewer_id)
      return nil if user.nil?

      viewer = ViewerContext.new(user:, friends: {}, friends_of_friends: {}, mutual_friends: Hash.new(0))
      viewer.has_posted_today = db.get_value!(<<~SQL, viewer.id, viewer.region)
        SELECT EXISTS(
          SELECT 1 FROM posts p
          JOIN posting_windows posted ON posted.id = p.posting_window_id
          JOIN posting_windows current ON current.region = $2 AND current.ended_at IS NULL
                                      AND current.local_date = posted.local_date
          WHERE p.author_id = $1 AND p.deleted_at IS NULL
        )
      SQL
      viewer.blocked = get_blocked_user_ids(db, viewer.id)
      friends = get_friend_ids(db, viewer.id)
      return viewer if friends.empty?

      friends.each { |id| viewer.friends[id] = true }
      friendships = db.select(
        "SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN (#{in_placeholders(1, friends.length)}) OR user_high_id IN (#{in_placeholders(friends.length + 1, friends.length)})",
        *friends, *friends
      )
      friendships.each do |friendship|
        low = friendship[:user_low_id]
        high = friendship[:user_high_id]
        if viewer.friends[low]
          viewer.friends_of_friends[high] = true
          viewer.mutual_friends[high] += 1 if high != viewer.id
        end
        if viewer.friends[high]
          viewer.friends_of_friends[low] = true
          viewer.mutual_friends[low] += 1 if low != viewer.id
        end
      end
      viewer
    end

    def can_view_post?(db, viewer_id, post_id)
      post = get_post_with_author(db, post_id)
      return false if post.nil?

      viewer = load_viewer_context(db, viewer_id)
      return false if viewer.nil?

      viewer.can_view?(post)
    end

    def get_friend_ids(db, user_id)
      db.select('SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id = $1 OR user_high_id = $1', user_id).map do |friendship|
        friendship[:user_low_id] == user_id ? friendship[:user_high_id] : friendship[:user_low_id]
      end
    end

    def get_blocked_user_ids(db, user_id)
      db.select_values(<<~SQL, user_id).to_h { |id| [id, true] }
        SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END
        FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
      SQL
    end
  end
end
