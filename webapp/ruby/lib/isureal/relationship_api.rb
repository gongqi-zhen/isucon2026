# frozen_string_literal: true

require 'securerandom'

module Isureal
  class App
    RELATIONSHIP_TARGET_REQUEST = {
      user_id: { param: 'userId' },
    }.freeze

    def delete_friendship_handler
      input = bind(RELATIONSHIP_TARGET_REQUEST)
      actor = current_session.user_id
      relationship_transaction(actor, input[:user_id]) do |tx|
        low, high = [actor, input[:user_id]].minmax
        count = tx.exec('DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2', low, high)
        require_relationship_change(count, 409, 'relationship_conflict')
      end
      no_content(204)
    end

    def follow_handler
      input = bind(RELATIONSHIP_TARGET_REQUEST)
      actor = current_session.user_id
      relationship_transaction(actor, input[:user_id]) do |tx|
        target = get_user(tx, input[:user_id]) or raise NoRowsError
        raise APIError.new(409, 'relationship_conflict') if target.account_type != 'certified_public'
        raise APIError.new(409, 'relationship_conflict') if blocked?(tx, actor, target.id)
        if tx.get_value!('SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id=$1 AND followee_id=$2)', actor, target.id)
          raise APIError.new(409, 'relationship_conflict')
        end

        tx.exec('INSERT INTO follows (id, follower_id, followee_id) VALUES ($1,$2,$3)', SecureRandom.uuid, actor, target.id)
      end
      no_content(204)
    end

    def unfollow_handler
      input = bind(RELATIONSHIP_TARGET_REQUEST)
      actor = current_session.user_id
      relationship_transaction(actor, input[:user_id]) do |tx|
        count = tx.exec('DELETE FROM follows WHERE follower_id=$1 AND followee_id=$2', actor, input[:user_id])
        require_relationship_change(count, 409, 'relationship_conflict')
      end
      no_content(204)
    end

    def block_handler
      input = bind(RELATIONSHIP_TARGET_REQUEST)
      actor = current_session.user_id
      relationship_transaction(actor, input[:user_id]) do |tx|
        if tx.get_value!('SELECT EXISTS(SELECT 1 FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2)', actor, input[:user_id])
          raise APIError.new(409, 'relationship_conflict')
        end

        tx.exec('INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES ($1,$2,$3)', SecureRandom.uuid, actor, input[:user_id])
        low, high = [actor, input[:user_id]].minmax
        tx.exec('DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2', low, high)
        tx.exec('DELETE FROM follows WHERE (follower_id=$1 AND followee_id=$2) OR (follower_id=$2 AND followee_id=$1)', actor, input[:user_id])
        tx.exec("UPDATE friend_requests SET status='cancelled', updated_at=clock_timestamp() WHERE status='pending' AND ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))",
                actor, input[:user_id])
      end
      no_content(204)
    end

    def unblock_handler
      input = bind(RELATIONSHIP_TARGET_REQUEST)
      actor = current_session.user_id
      relationship_transaction(actor, input[:user_id]) do |tx|
        count = tx.exec('DELETE FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2', actor, input[:user_id])
        require_relationship_change(count, 403, 'relationship_action_forbidden')
      end
      no_content(204)
    end
  end
end
