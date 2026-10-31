# frozen_string_literal: true

require 'securerandom'

module Isureal
  class App
    REACTION_TYPES = %w[like smile surprise heart laugh instant].freeze

    REPLACE_REAL_MOJI_REQUEST = {
      post_id: { param: 'postId' },
      image: { form: 'image', type: :file },
      reaction_type: { form: 'reactionType' },
    }.freeze

    GET_REAL_MOJIS_REQUEST = {
      post_id: { param: 'postId' },
      before_created_at: { query: 'beforeCreatedAt', type: :time },
      before_id: { query: 'beforeId' },
    }.freeze

    DELETE_REAL_MOJI_REQUEST = {
      real_moji_id: { param: 'realMojiId' },
    }.freeze

    def realmoji_response(reaction, user)
      {
        id: reaction.id, reactor: relationship_user_response(user), reactionType: reaction.reaction_type,
        imageUrl: ImageSignature.signed_url(user, reaction.image_id), createdAt: Response.format_time(reaction.created_at),
      }
    end

    def replace_real_moji_handler
      input = bind(REPLACE_REAL_MOJI_REQUEST)
      raise APIError.new(400, 'invalid_realmoji') if input[:image].nil?
      raise APIError.new(400, 'invalid_realmoji') unless REACTION_TYPES.include?(input[:reaction_type])

      input[:image].rewind
      data = input[:image].read
      image = JPEG.decode(data)
      raise APIError.new(400, 'invalid_realmoji') if image.nil?
      raise APIError.new(400, 'invalid_realmoji') if image.width > 512 || image.height > 512

      actor = current_session.user_id
      post = get_post(db, input[:post_id])
      raise APIError.new(403, 'interaction_forbidden') if post.nil?
      raise APIError.new(403, 'interaction_forbidden') if actor == post.author_id

      reaction, user = relationship_transaction(actor, post.author_id) do |tx|
        raise APIError.new(403, 'interaction_forbidden') unless can_view_post?(tx, actor, post.id)

        tx.exec('UPDATE realmojis SET replaced_at=clock_timestamp() WHERE post_id=$1 AND reactor_id=$2 AND replaced_at IS NULL AND deleted_at IS NULL', post.id, actor)
        image_id = SecureRandom.uuid
        tx.exec("INSERT INTO image_objects(id,owner_user_id,data,media_type) VALUES($1,$2,$3,'image/jpeg')", image_id, actor, data)
        id = SecureRandom.uuid
        tx.exec('INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at) VALUES($1,$2,$3,$4,$5,clock_timestamp())',
                id, post.id, actor, image_id, input[:reaction_type])
        reaction = get_real_moji(tx, id) or raise NoRowsError
        user = get_user(tx, actor) or raise NoRowsError
        [reaction, user]
      end
      json_response(201, realmoji_response(reaction, user))
    end

    def get_real_mojis_handler
      input = bind(GET_REAL_MOJIS_REQUEST)
      raise APIError.new(400, 'invalid_pagination') if input[:before_created_at].nil? != input[:before_id].empty?
      raise APIError.new(403, 'interaction_forbidden') unless can_view_post?(db, current_session.user_id, input[:post_id])

      reactions = select_current_realmojis(db, 'AND r.post_id=$1 AND ($2::timestamptz IS NULL OR (r.created_at,r.id)<($2,$3)) ORDER BY r.created_at DESC,r.id DESC LIMIT 51',
                                           input[:post_id], input[:before_created_at], input[:before_id])
      items = reactions.map { |reaction| realmoji_response(reaction, reaction.reactor) }
      has_more = items.length > 50
      items = items.first(50) if has_more
      json_response(200, { items:, hasMore: has_more })
    end

    def delete_real_moji_handler
      input = bind(DELETE_REAL_MOJI_REQUEST)
      actor = current_session.user_id
      db.transaction do |tx|
        tx.get_value!('SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE', actor)
        record = tx.get('SELECT r.reactor_id,p.author_id FROM realmojis r JOIN posts p ON p.id=r.post_id WHERE r.id=$1 AND r.deleted_at IS NULL FOR UPDATE OF r', input[:real_moji_id])
        raise APIError.new(404, 'interaction_not_found') if record.nil?
        raise APIError.new(403, 'interaction_forbidden') if actor != record[:reactor_id] && actor != record[:author_id]

        tx.exec('UPDATE realmojis SET deleted_at=clock_timestamp() WHERE id=$1', input[:real_moji_id])
      end
      no_content(204)
    end
  end
end
