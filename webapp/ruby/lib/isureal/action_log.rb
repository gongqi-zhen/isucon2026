# frozen_string_literal: true

require 'mustermann'

module Isureal
  class ActionLog
    ACTION_NAMES = {
      'POST /api/initialize' => 'initialize',
      'POST /api/auth/register' => 'register',
      'POST /api/auth/login' => 'login',
      'POST /api/auth/logout' => 'logout',
      'GET /api/users' => 'search_users',
      'GET /api/users/:userId' => 'get_user',
      'GET /api/me/export' => 'export_posts',
      'GET /api/me' => 'get_me',
      'PATCH /api/me' => 'update_me',
      'PUT /api/me/profile-image' => 'put_profile_image',
      'DELETE /api/me/profile-image' => 'delete_profile_image',
      'POST /api/me/account-deletion' => 'delete_account',
      'POST /api/posts' => 'create_post',
      'GET /api/posting-window/current' => 'get_current_posting_window',
      'GET /api/posts/me/current' => 'get_my_current_post',
      'GET /api/posts/:postId' => 'get_post',
      'PATCH /api/posts/:postId' => 'update_post',
      'DELETE /api/posts/:postId' => 'delete_post',
      'GET /api/feeds/:feed' => 'get_feed',
      'GET /api/memories' => 'get_memories',
      'GET /api/memories/:date' => 'get_memories_by_date',
      'POST /api/posts/:postId/comments' => 'create_comment',
      'GET /api/posts/:postId/comments' => 'get_comments',
      'DELETE /api/comments/:commentId' => 'delete_comment',
      'POST /api/posts/:postId/realmojis' => 'replace_real_moji',
      'GET /api/posts/:postId/realmojis' => 'get_real_mojis',
      'DELETE /api/realmojis/:realMojiId' => 'delete_real_moji',
      'GET /api/images/:imageId' => 'get_image',
      'GET /api/notifications' => 'get_notifications',
      'POST /api/friend-requests' => 'create_friend_request',
      'POST /api/friend-requests/:requestId/:action' => 'transition_friend_request',
      'GET /api/relationships/:kind' => 'get_relationships',
      'DELETE /api/friendships/:userId' => 'delete_friendship',
      'PUT /api/follows/:userId' => 'follow',
      'DELETE /api/follows/:userId' => 'unfollow',
      'PUT /api/blocks/:userId' => 'block',
      'DELETE /api/blocks/:userId' => 'unblock',
      'POST /api/admin/auth/login' => 'admin_login',
      'POST /api/admin/auth/logout' => 'admin_logout',
      'PUT /api/admin/users/:userId/certification' => 'certify_user',
      'DELETE /api/admin/users/:userId/certification' => 'delete_user_certification',
      'POST /api/admin/posting-window' => 'open_posting_window',
    }.freeze

    ROUTE_PATTERNS = ACTION_NAMES.keys.map do |route|
      method, path = route.split(' ', 2)
      [method, Mustermann.new(path), route]
    end.freeze

    LOG_LEVEL_RANKS = { 'debug' => 0, 'info' => 1, 'error' => 2 }.freeze

    def initialize(app, min_level:, output:)
      @app = app
      @min_rank = LOG_LEVEL_RANKS.fetch(min_level, 0)
      @output = output
    end

    def call(env)
      status, headers, body = @app.call(env)
      write(env, status)
      [status, headers, body]
    end

    private

    def write(env, status)
      level = if status >= 500
                'error'
              elsif env['REQUEST_METHOD'] == 'GET' && !(status == 403 && env['isureal.abuse'] == 'invalid_signature')
                'debug'
              else
                'info'
              end
      return if LOG_LEVEL_RANKS.fetch(level) < @min_rank

      action = ACTION_NAMES[matched_route(env)]
      entry = { action:, ip: IPBan.extract_client_ip(env), level:, status:, user_id: env['isureal.session']&.user_id }
      if %w[login register].include?(action)
        username = env['isureal.username']
        entry[:username] = username.is_a?(String) && USERNAME_PATTERN.match?(username) ? username : nil
      end
      entry[:abuse] = env['isureal.abuse'] if env['isureal.abuse']
      @output.write(Response.encode_json(entry))
    end

    def matched_route(env)
      return env['sinatra.route'] if env['sinatra.route']

      method = env['REQUEST_METHOD']
      path = env['PATH_INFO']
      ROUTE_PATTERNS.find { |route_method, pattern, _| route_method == method && pattern.match(path) }&.last
    end
  end
end
