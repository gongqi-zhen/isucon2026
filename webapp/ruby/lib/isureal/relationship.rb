# frozen_string_literal: true

require 'securerandom'

module Isureal
  FriendRequest = Struct.new(:id, :requester_id, :recipient_id, :status, :created_at, :updated_at, keyword_init: true)

  class App
    def get_friend_request(db, id)
      row = db.get('SELECT id, requester_id, recipient_id, status, created_at, updated_at FROM friend_requests WHERE id=$1', id)
      row && FriendRequest.new(**row)
    end

    def relationship_transaction(actor, target)
      raise APIError.new(400, 'invalid_relationship') if actor == target || target.to_s.empty?

      db.transaction do |tx|
        [actor, target].minmax.each do |id|
          locked_id = tx.get_value("SELECT id FROM users WHERE id=$1 AND status='active' FOR NO KEY UPDATE", id)
          raise APIError.new(404, 'relationship_not_found') if locked_id.nil?
        end
        yield tx
      end
    end

    def blocked?(db, actor, target)
      db.get_value!('SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))', actor, target)
    end

    def insert_friendship(tx, request)
      low, high = [request.requester_id, request.recipient_id].minmax
      tx.exec('INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id) VALUES ($1,$2,$3,$4)', SecureRandom.uuid, low, high, request.id)
    end

    def require_relationship_change(count, status, code)
      raise APIError.new(status, code) if count.zero?
    end
  end
end
