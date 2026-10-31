# frozen_string_literal: true

require 'date'

module Isureal
  class App
    GET_MEMORIES_REQUEST = {
      month: { query: 'month' },
      before_posted_at: { query: 'beforePostedAt', type: :time },
      before_id: { query: 'beforeId' },
    }.freeze

    GET_MEMORIES_BY_DATE_REQUEST = {
      date: { param: 'date' },
      before_posted_at: { query: 'beforePostedAt', type: :time },
      before_id: { query: 'beforeId' },
    }.freeze

    def parse_month(value)
      match = /\A(\d{4})-(\d{2})\z/.match(value)
      match && Date.valid_date?(match[1].to_i, match[2].to_i, 1) ? Date.new(match[1].to_i, match[2].to_i, 1) : nil
    end

    def parse_date(value)
      match = /\A(\d{4})-(\d{2})-(\d{2})\z/.match(value)
      date = match&.captures&.map(&:to_i)
      date && Date.valid_date?(*date) ? Date.new(*date) : nil
    end

    def get_memories_handler
      input = bind(GET_MEMORIES_REQUEST)
      start = parse_month(input[:month])
      raise APIError.new(400, 'invalid_memory_period') if start.nil?

      response = list_memories(db, current_session.user_id, start, start >> 1, input[:before_posted_at], input[:before_id])
      json_response(200, response)
    end

    def get_memories_by_date_handler
      input = bind(GET_MEMORIES_BY_DATE_REQUEST)
      start = parse_date(input[:date])
      raise APIError.new(400, 'invalid_memory_period') if start.nil?

      response = list_memories(db, current_session.user_id, start, start + 1, input[:before_posted_at], input[:before_id])
      json_response(200, response)
    end

    def list_memories(db, user_id, start, finish, before_at, before_id)
      raise APIError.new(400, 'invalid_pagination') if before_at.nil? != before_id.empty?

      posts = select_posts_with_author(db, <<~SQL, user_id, start, finish, before_at, before_id)
        AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
        AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
        ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
      SQL
      items = posts.map { |post| post_response(post, post.author).merge(realmojis: [], comments: []) }
      has_more = items.length > 50
      items = items.first(50) if has_more
      load_memory_interactions(db, items)
      { items:, hasMore: has_more }
    end

    def load_memory_interactions(db, items)
      return if items.empty?

      post_ids = items.map { |item| item[:id] }
      item_by_post_id = items.to_h { |item| [item[:id], item] }

      reactions = select_current_realmojis(db, "AND r.post_id IN (#{in_placeholders(1, post_ids.length)}) ORDER BY r.created_at DESC, r.id DESC", *post_ids)
      reactions.each do |reaction|
        item = item_by_post_id[reaction.post_id]
        item[:realmojis] << realmoji_response(reaction, reaction.reactor) if item[:realmojis].length < 50
      end

      comments = select_comments_with_author(db, "AND c.post_id IN (#{in_placeholders(1, post_ids.length)}) ORDER BY c.created_at, c.id", *post_ids)
      comments.each do |comment|
        item = item_by_post_id[comment.post_id]
        item[:comments] << comment_response(comment, comment.author) if item[:comments].length < 50
      end
    end
  end
end
