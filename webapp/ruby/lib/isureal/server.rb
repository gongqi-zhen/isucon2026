# frozen_string_literal: true

module Isureal
  def self.new_server(config, pool_size:, output: $stdout)
    raise ArgumentError, "invalid server region: #{config.region}" unless UserValidation.valid_region?(config.region)

    db = Database.new(config.database_options, size: pool_size)
    handler_context = HandlerContext.new(db:, config:, ip_bans: IPBanList.new)
    ActionLog.new(App.new(handler_context:), min_level: config.log_level, output:)
  end

  class App
    before { ip_ban_middleware }

    post('/api/initialize') { initialize_handler }
    post('/api/auth/register') { register_handler }
    post('/api/auth/login') { login_handler }
    post('/api/auth/logout') { session_middleware(true) { logout_handler } }
    get('/api/users') { session_middleware(true) { region_middleware { search_users_handler } } }
    get('/api/users/:userId') { session_middleware(true) { region_middleware { get_user_handler } } }
    get('/api/me/export') { session_middleware(true) { region_middleware { export_posts_handler } } }
    get('/api/me') { session_middleware(true) { region_middleware { get_me_handler } } }
    patch('/api/me') { session_middleware(true) { update_me_handler } }
    put('/api/me/profile-image') { session_middleware(true) { put_profile_image_handler } }
    delete('/api/me/profile-image') { session_middleware(true) { delete_profile_image_handler } }
    post('/api/me/account-deletion') { session_middleware(true) { region_middleware { delete_account_handler } } }
    post('/api/posts') { session_middleware(true) { region_middleware { create_post_handler } } }
    get('/api/posting-window/current') { session_middleware(true) { region_middleware { get_current_posting_window_handler } } }
    get('/api/posts/me/current') { session_middleware(true) { region_middleware { get_my_current_post_handler } } }
    get('/api/posts/:postId') { session_middleware(true) { region_middleware { get_post_handler } } }
    patch('/api/posts/:postId') { session_middleware(true) { region_middleware { update_post_handler } } }
    delete('/api/posts/:postId') { session_middleware(true) { region_middleware { delete_post_handler } } }
    get('/api/feeds/:feed') { session_middleware(true) { region_middleware { get_feed_handler } } }
    get('/api/memories') { session_middleware(true) { region_middleware { get_memories_handler } } }
    get('/api/memories/:date') { session_middleware(true) { region_middleware { get_memories_by_date_handler } } }
    post('/api/posts/:postId/comments') { session_middleware(true) { region_middleware { create_comment_handler } } }
    get('/api/posts/:postId/comments') { session_middleware(true) { region_middleware { get_comments_handler } } }
    delete('/api/comments/:commentId') { session_middleware(true) { region_middleware { delete_comment_handler } } }
    post('/api/posts/:postId/realmojis') { session_middleware(true) { region_middleware { replace_real_moji_handler } } }
    get('/api/posts/:postId/realmojis') { session_middleware(true) { region_middleware { get_real_mojis_handler } } }
    delete('/api/realmojis/:realMojiId') { session_middleware(true) { region_middleware { delete_real_moji_handler } } }
    get('/api/images/:imageId') { get_image_handler }
    get('/api/notifications') { session_middleware(true) { region_middleware { get_notifications_handler } } }
    post('/api/friend-requests') { session_middleware(true) { region_middleware { create_friend_request_handler } } }
    post('/api/friend-requests/:requestId/:action') { session_middleware(true) { region_middleware { transition_friend_request_handler } } }
    get('/api/relationships/:kind') { session_middleware(true) { region_middleware { get_relationships_handler } } }
    delete('/api/friendships/:userId') { session_middleware(true) { region_middleware { delete_friendship_handler } } }
    put('/api/follows/:userId') { session_middleware(true) { region_middleware { follow_handler } } }
    delete('/api/follows/:userId') { session_middleware(true) { region_middleware { unfollow_handler } } }
    put('/api/blocks/:userId') { session_middleware(true) { region_middleware { block_handler } } }
    delete('/api/blocks/:userId') { session_middleware(true) { region_middleware { unblock_handler } } }
    post('/api/admin/auth/login') { admin_login_handler }
    post('/api/admin/auth/logout') { admin_session_middleware(true) { admin_logout_handler } }
    put('/api/admin/users/:userId/certification') { admin_session_middleware(true) { certify_user_handler } }
    delete('/api/admin/users/:userId/certification') { admin_session_middleware(true) { delete_user_certification_handler } }
    post('/api/admin/posting-window') { admin_session_middleware(true) { open_posting_window_handler } }
  end
end
