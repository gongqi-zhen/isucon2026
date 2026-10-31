# frozen_string_literal: true

module Isureal
  class App
    def get_current_posting_window_handler
      state = db.get(<<~SQL, current_session.user_id)
        SELECT w.id, w.region, w.local_date, w.starts_at, w.on_time_deadline, w.ended_at,
               count(p.id) FILTER (WHERE p.deleted_at IS NULL) AS active,
               count(p.id) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
        FROM users u JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
        LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date
        LEFT JOIN posts p ON p.author_id = u.id AND p.posting_window_id = posted_window.id
        WHERE u.id = $1
        GROUP BY w.id
      SQL
      return json_response(200, { window: nil, hasPosted: false, canPost: false, deletionCount: 0 }) if state.nil?

      json_response(200, {
        window: {
          id: state[:id], region: state[:region], localDate: state[:local_date].iso8601,
          startsAt: Response.format_time(state[:starts_at]), onTimeDeadline: Response.format_time(state[:on_time_deadline]),
        },
        hasPosted: state[:active].positive?,
        canPost: state[:active].zero? && state[:deleted] < 2 && Time.now >= state[:starts_at],
        deletionCount: state[:deleted],
      })
    end
  end
end
