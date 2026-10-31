# frozen_string_literal: true

require 'securerandom'

module Isureal
  class App
    CREATE_FRIEND_REQUEST_REQUEST = {
      recipient_id: { json: 'recipientId' },
    }.freeze

    TRANSITION_FRIEND_REQUEST_REQUEST = {
      request_id: { param: 'requestId' },
      action: { param: 'action' },
    }.freeze

    FRIEND_REQUEST_TRANSITIONS = {
      'accept' => 'accepted',
      'reject' => 'rejected',
      'cancel' => 'cancelled',
    }.freeze

    def friend_request_response(request)
      { relationId: request.id, status: request.status, updatedAt: Response.format_time(request.updated_at) }
    end

    def create_friend_request_handler
      input = bind(CREATE_FRIEND_REQUEST_REQUEST)
      actor = current_session.user_id
      recipient_id = input[:recipient_id]
      request = relationship_transaction(actor, recipient_id) do |tx|
        raise APIError.new(409, 'relationship_conflict') if blocked?(tx, actor, recipient_id)
        raise APIError.new(409, 'relationship_conflict') if get_friend_ids(tx, actor).include?(recipient_id)

        existing_id = tx.get_value(<<~SQL, actor, recipient_id)
          SELECT id FROM friend_requests WHERE status='pending' AND
           ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))
        SQL
        if existing_id
          request = get_friend_request(tx, existing_id) or raise NoRowsError
          raise APIError.new(409, 'relationship_conflict') if request.requester_id == actor

          tx.exec("UPDATE friend_requests SET status='accepted', updated_at=clock_timestamp() WHERE id=$1", request.id)
          insert_friendship(tx, request)
          request_id = request.id
        else
          request_id = SecureRandom.uuid
          tx.exec("INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at) VALUES ($1,$2,$3,'pending',clock_timestamp(),clock_timestamp())",
                  request_id, actor, recipient_id)
        end
        get_friend_request(tx, request_id) or raise NoRowsError
      end
      json_response(201, friend_request_response(request))
    end

    def transition_friend_request_handler
      input = bind(TRANSITION_FRIEND_REQUEST_REQUEST)
      status = FRIEND_REQUEST_TRANSITIONS[input[:action]]
      raise APIError.new(403, 'relationship_action_forbidden') if status.nil?

      actor = current_session.user_id
      request = get_friend_request(db, input[:request_id])
      raise APIError.new(404, 'relationship_not_found') if request.nil?
      if (status == 'cancelled' && actor != request.requester_id) || (status != 'cancelled' && actor != request.recipient_id)
        raise APIError.new(403, 'relationship_action_forbidden')
      end

      request = relationship_transaction(request.requester_id, request.recipient_id) do |tx|
        request = get_friend_request(tx, input[:request_id]) or raise NoRowsError
        raise APIError.new(403, 'relationship_action_forbidden') if request.status != 'pending'

        if status == 'accepted'
          raise APIError.new(409, 'relationship_conflict') if blocked?(tx, request.requester_id, request.recipient_id)

          insert_friendship(tx, request)
        end
        tx.exec('UPDATE friend_requests SET status=$2, updated_at=clock_timestamp() WHERE id=$1', request.id, status)
        get_friend_request(tx, request.id) or raise NoRowsError
      end
      json_response(200, friend_request_response(request))
    end
  end
end
