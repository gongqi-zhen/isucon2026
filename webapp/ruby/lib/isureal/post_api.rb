# frozen_string_literal: true

require 'securerandom'

module Isureal
  class App
    VISIBILITIES = %w[friends friends_of_friends public].freeze

    CREATE_POST_REQUEST = {
      image: { form: 'image', type: :file },
      caption: { form: 'caption' },
      visibility: { form: 'visibility' },
      location_kind: { form: 'location_kind' },
      latitude: { form: 'latitude', type: :float },
      longitude: { form: 'longitude', type: :float },
      locality: { form: 'locality', type: :string_pointer },
    }.freeze

    GET_POST_REQUEST = {
      post_id: { param: 'postId' },
    }.freeze

    UPDATE_POST_REQUEST = {
      post_id: { param: 'postId' },
      caption: { json: 'caption', type: :string_pointer },
      visibility: { json: 'visibility', type: :string_pointer },
    }.freeze

    DELETE_POST_REQUEST = {
      post_id: { param: 'postId' },
    }.freeze

    def create_post_handler
      input = bind(CREATE_POST_REQUEST)
      raise APIError.new(400, 'invalid_post') if input[:image].nil? || !valid_user_text?(input[:caption], 0, 1000)
      raise APIError.new(400, 'invalid_post') unless VISIBILITIES.include?(input[:visibility])

      location = {
        kind: input[:location_kind], latitude: input[:latitude],
        longitude: input[:longitude], locality: input[:locality],
      }
      raise APIError.new(400, 'invalid_post') unless PostLocation.valid?(**location)

      input[:image].rewind
      image_data = input[:image].read
      image = JPEG.decode(image_data)
      raise APIError.new(400, 'invalid_post') if image.nil?
      raise APIError.new(400, 'invalid_post') if image.width > 4096 || image.height > 4096

      user_id = current_session.user_id
      user, post = db.transaction do |tx|
        tx.get_value!('SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE', user_id)
        user = get_user(tx, user_id) or raise NoRowsError
        window_query = 'SELECT id, local_date, starts_at FROM posting_windows WHERE region = $1 AND ended_at IS NULL FOR UPDATE'
        window = tx.get(window_query, user.region) || tx.get(window_query, user.region)
        raise APIError.new(409, 'post_slot_unavailable') if window.nil?

        counts = tx.get!(<<~SQL, user_id, window[:local_date])
          SELECT count(*) FILTER (WHERE deleted_at IS NULL) AS active,
                 count(*) FILTER (WHERE deleted_at IS NOT NULL) AS deleted
          FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
          WHERE p.author_id = $1 AND w.local_date = $2
        SQL
        posted_at = Time.now.utc
        if counts[:active] != 0 || counts[:deleted] >= 2 || posted_at < window[:starts_at]
          raise APIError.new(409, 'post_slot_unavailable')
        end

        image_id = SecureRandom.uuid
        tx.exec("INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES ($1, $2, $3, 'image/jpeg', $4)",
                image_id, user_id, image_data, posted_at)
        post_id = SecureRandom.uuid
        insert_post = <<~SQL
          INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                             location_kind, latitude, longitude, locality, posted_at)
          VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
        SQL
        tx.exec(insert_post, post_id, user_id, window[:id], image_id, input[:caption], input[:visibility],
                location[:kind], location[:latitude], location[:longitude], location[:locality], posted_at)
        post = get_post(tx, post_id) or raise NoRowsError
        [user, post]
      end
      json_response(201, post_response(post, user))
    end

    def post_response(post, user)
      on_time = post.posted_at <= post.on_time_deadline
      {
        id: post.id,
        author: {
          id: user.id, username: user.username, displayName: user.display_name,
          profileImageUrl: profile_image_url(user), bio: user.bio, accountType: user.account_type,
        },
        postingWindowId: post.posting_window_id,
        imageUrl: ImageSignature.signed_url(user, post.image_id),
        caption: post.caption,
        visibility: post.visibility,
        location: post.location,
        postedAt: Response.format_time(post.posted_at),
        onTime: on_time,
        lateSeconds: on_time ? 0 : (post.posted_at.to_r - post.on_time_deadline.to_r).truncate,
      }
    end

    def get_my_current_post_handler
      user_id = current_session.user_id
      post_id = db.get_value(<<~SQL, user_id)
        SELECT p.id FROM posts p
        JOIN posting_windows w ON w.id = p.posting_window_id
        JOIN users u ON u.id = p.author_id
        JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                   AND current.local_date = w.local_date
        WHERE p.author_id = $1 AND p.deleted_at IS NULL
      SQL
      raise APIError.new(404, 'post_not_found') if post_id.nil?

      post = get_post(db, post_id)
      raise APIError.new(404, 'post_not_found') if post.nil?

      user = get_user(db, user_id) or raise NoRowsError
      json_response(200, post_response(post, user))
    end

    def get_post_handler
      input = bind(GET_POST_REQUEST)
      post = get_post_with_author(db, input[:post_id])
      raise APIError.new(404, 'post_not_found') if post.nil?

      viewer = load_viewer_context(db, current_session.user_id)
      raise APIError.new(404, 'post_not_found') if viewer.nil? || !viewer.can_view?(post)

      json_response(200, post_response(post, post.author))
    end

    def update_post_handler
      input = bind(UPDATE_POST_REQUEST)
      raise APIError.new(400, 'invalid_post') if input[:caption].nil? && input[:visibility].nil?
      raise APIError.new(400, 'invalid_post') if !input[:caption].nil? && !valid_user_text?(input[:caption], 0, 1000)
      raise APIError.new(400, 'invalid_post') if !input[:visibility].nil? && !VISIBILITIES.include?(input[:visibility])

      user_id = current_session.user_id
      user, post = db.transaction do |tx|
        tx.get_value!('SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE', user_id)
        post = get_post(tx, input[:post_id])
        raise APIError.new(404, 'post_not_found') if post.nil?
        raise APIError.new(403, 'post_not_editable') if post.author_id != user_id

        window = get_posting_window(tx, post.posting_window_id) or raise NoRowsError
        raise APIError.new(403, 'post_not_editable') unless window.ended_at.nil?

        tx.exec('UPDATE posts SET caption = $2 WHERE id = $1', post.id, input[:caption]) unless input[:caption].nil?
        tx.exec('UPDATE posts SET visibility = $2 WHERE id = $1', post.id, input[:visibility]) unless input[:visibility].nil?
        post = get_post(tx, post.id) or raise NoRowsError
        user = get_user(tx, user_id) or raise NoRowsError
        [user, post]
      end
      json_response(200, post_response(post, user))
    end

    def delete_post_handler
      input = bind(DELETE_POST_REQUEST)
      user_id = current_session.user_id
      db.transaction do |tx|
        tx.get_value!('SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE', user_id)
        post = get_post(tx, input[:post_id])
        raise APIError.new(404, 'post_not_found') if post.nil?
        raise APIError.new(403, 'post_not_editable') if post.author_id != user_id

        window = get_posting_window(tx, post.posting_window_id) or raise NoRowsError
        deletions = tx.get_value!(<<~SQL, user_id, window.local_date)
          SELECT count(*) FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
          WHERE p.author_id = $1 AND w.local_date = $2 AND p.deleted_at IS NOT NULL
        SQL
        raise APIError.new(409, 'post_slot_unavailable') if deletions >= 2

        tx.exec('UPDATE posts SET deleted_at = clock_timestamp() WHERE id = $1', post.id)
      end
      no_content(204)
    end
  end
end
