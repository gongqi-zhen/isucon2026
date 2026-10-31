# frozen_string_literal: true

require 'securerandom'

module Isureal
  class App
    OPEN_POSTING_WINDOW_REQUEST = {
      region: { json: 'region' },
      local_date: { json: 'localDate' },
      on_time_seconds: { json: 'onTimeSeconds', type: :int },
    }.freeze

    def open_posting_window_handler
      input = bind(OPEN_POSTING_WINDOW_REQUEST)
      region = input[:region]
      local_date = parse_date(input[:local_date])
      raise APIError.new(400, 'invalid_posting_window') if !valid_region?(region) || local_date.nil?

      seconds = input[:on_time_seconds] || (24 * 60 * 60)
      window = db.transaction do |tx|
        tx.exec('LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE')
        if tx.get_value!('SELECT EXISTS(SELECT 1 FROM posting_windows WHERE region = $1 AND local_date >= $2)', region, local_date)
          raise APIError.new(409, 'posting_window_conflict')
        end

        starts_at = tx.get_value!('SELECT clock_timestamp()')
        tx.exec('UPDATE posting_windows SET ended_at = $2 WHERE region = $1 AND ended_at IS NULL', region, starts_at)
        window_id = SecureRandom.uuid
        tx.exec(<<~SQL, window_id, region, local_date, starts_at, starts_at + seconds)
          INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
          VALUES ($1, $2, $3, $4, $5)
        SQL
        tx.exec(<<~SQL, region, window_id, starts_at)
          INSERT INTO notifications (id, user_id, posting_window_id, created_at)
          SELECT gen_random_uuid()::text, id, $2, $3 FROM users WHERE region = $1 AND status = 'active'
        SQL
        get_posting_window(tx, window_id) or raise NoRowsError
      end
      json_response(201, {
        id: window.id, region: window.region, localDate: window.local_date.iso8601,
        startsAt: Response.format_time(window.starts_at), onTimeDeadline: Response.format_time(window.on_time_deadline),
      })
    end
  end
end
