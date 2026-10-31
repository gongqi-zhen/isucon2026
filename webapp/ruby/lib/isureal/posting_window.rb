# frozen_string_literal: true

module Isureal
  PostingWindow = Struct.new(:id, :region, :local_date, :starts_at, :on_time_deadline, :ended_at, keyword_init: true)

  class App
    def get_posting_window(db, window_id)
      row = db.get('SELECT id, region, local_date, starts_at, on_time_deadline, ended_at FROM posting_windows WHERE id = $1', window_id)
      row && PostingWindow.new(**row)
    end
  end
end
