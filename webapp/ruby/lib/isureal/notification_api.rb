# frozen_string_literal: true

require 'io/wait'

module Isureal
  class App
    NOTIFICATION_POLL_INTERVAL = 0.5

    def get_notifications_handler
      session = current_session
      user_id = session.user_id
      connected_at = db.get_value!('SELECT clock_timestamp()')
      status 200
      headers['content-type'] = 'text/event-stream'
      body(lambda do |stream|
        Thread.new do
          stream_notification(stream, session, user_id, connected_at)
        ensure
          begin
            stream.close
          rescue IOError, SystemCallError
            nil
          end
        end
      end)
    end

    def stream_notification(stream, session, user_id, connected_at)
      next_tick = monotonic_now + NOTIFICATION_POLL_INTERVAL
      loop do
        begin
          user = get_user(db, user_id)
        rescue StandardError => e
          Log.error('failed to get notification recipient', e.message)
          return
        end
        return if user.nil? || user.region != session.region

        begin
          notification = db.get(<<~SQL, user_id, connected_at)
            SELECT n.id, n.posting_window_id, w.region, n.created_at
            FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
            WHERE n.user_id = $1 AND n.created_at > $2 AND w.ended_at IS NULL
            ORDER BY n.created_at, n.id LIMIT 1
          SQL
        rescue StandardError => e
          Log.error('failed to get notification', e.message)
          return
        end
        unless notification.nil?
          data = Response.encode_json({
            id: notification[:id], postingWindowId: notification[:posting_window_id],
            region: notification[:region], createdAt: Response.format_time(notification[:created_at]),
          }).chomp
          stream.write("data: #{data}\n\n")
          return
        end

        now = monotonic_now
        if now < next_tick
          return if stream.wait_readable(next_tick - now)

          next_tick += NOTIFICATION_POLL_INTERVAL
        else
          next_tick += NOTIFICATION_POLL_INTERVAL while next_tick <= now
        end
      end
    rescue IOError, SystemCallError
      nil
    end

    def monotonic_now = Process.clock_gettime(Process::CLOCK_MONOTONIC)
  end
end
