# frozen_string_literal: true

require 'securerandom'

module Isureal
  class App
    CREATE_COMMENT_REQUEST = {
      post_id: { param: 'postId' },
      body: { json: 'body' },
    }.freeze

    GET_COMMENTS_REQUEST = {
      post_id: { param: 'postId' },
      after_created_at: { query: 'afterCreatedAt', type: :time },
      after_id: { query: 'afterId' },
    }.freeze

    DELETE_COMMENT_REQUEST = {
      comment_id: { param: 'commentId' },
    }.freeze

    def comment_response(comment, user)
      { id: comment.id, author: relationship_user_response(user), body: comment.body, createdAt: Response.format_time(comment.created_at) }
    end

    def create_comment_handler
      input = bind(CREATE_COMMENT_REQUEST)
      raise APIError.new(400, 'invalid_interaction') unless valid_user_text?(input[:body], 1, 1000)

      actor = current_session.user_id
      post = get_post(db, input[:post_id])
      raise APIError.new(403, 'interaction_forbidden') if post.nil?

      comment, user = db.transaction do |tx|
        [actor, post.author_id].minmax.uniq.each do |id|
          tx.get_value!('SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE', id)
        end
        raise APIError.new(403, 'interaction_forbidden') unless can_comment?(tx, actor, post.id)

        id = SecureRandom.uuid
        tx.exec('INSERT INTO comments(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,clock_timestamp())', id, post.id, actor, input[:body])
        comment = get_comment(tx, id) or raise NoRowsError
        user = get_user(tx, actor) or raise NoRowsError
        [comment, user]
      end
      json_response(201, comment_response(comment, user))
    end

    def get_comments_handler
      input = bind(GET_COMMENTS_REQUEST)
      raise APIError.new(400, 'invalid_pagination') if input[:after_created_at].nil? != input[:after_id].empty?
      raise APIError.new(403, 'interaction_forbidden') unless can_comment?(db, current_session.user_id, input[:post_id])

      comments = select_comments_with_author(db, 'AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3)) ORDER BY c.created_at,c.id LIMIT 51',
                                             input[:post_id], input[:after_created_at], input[:after_id])
      items = comments.map { |comment| comment_response(comment, comment.author) }
      has_more = items.length > 50
      items = items.first(50) if has_more
      json_response(200, { items:, hasMore: has_more })
    end

    def delete_comment_handler
      input = bind(DELETE_COMMENT_REQUEST)
      actor = current_session.user_id
      db.transaction do |tx|
        tx.get_value!('SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE', actor)
        record = tx.get('SELECT c.author_id,p.author_id AS post_author_id FROM comments c JOIN posts p ON p.id=c.post_id WHERE c.id=$1 AND c.deleted_at IS NULL FOR UPDATE OF c', input[:comment_id])
        raise APIError.new(404, 'interaction_not_found') if record.nil?
        raise APIError.new(403, 'interaction_forbidden') if actor != record[:author_id] && actor != record[:post_author_id]

        tx.exec('UPDATE comments SET deleted_at=clock_timestamp() WHERE id=$1', input[:comment_id])
      end
      no_content(204)
    end
  end
end
