# frozen_string_literal: true

module Isureal
  class APIError < StandardError
    attr_reader :status, :code

    def initialize(status, code)
      super(code)
      @status = status
      @code = code
    end
  end

  class HTTPError < StandardError
    STATUS_TEXT = {
      400 => 'Bad Request',
      403 => 'Forbidden',
      404 => 'Not Found',
      405 => 'Method Not Allowed',
      415 => 'Unsupported Media Type',
    }.freeze

    attr_reader :status

    def initialize(status, message = nil)
      super(message || STATUS_TEXT.fetch(status))
      @status = status
    end

    def self.wrap(status, error)
      new(status, "code=#{status}, message=#{STATUS_TEXT.fetch(status)}, err=#{error}")
    end

    def self.bad_request(error) = wrap(400, error)

    def self.unsupported_media_type = new(415, 'code=415, message=Unsupported Media Type')
  end

  class NoRowsError < StandardError
    def initialize(message = 'sql: no rows in result set')
      super
    end
  end
end
