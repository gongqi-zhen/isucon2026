# frozen_string_literal: true

module Isureal
  class App
    def region_middleware
      session = current_session
      raise APIError.new(401, 'invalid_session') if session.nil?
      raise APIError.new(403, 'region_mismatch') if session.region != config.region

      yield
    end
  end
end
