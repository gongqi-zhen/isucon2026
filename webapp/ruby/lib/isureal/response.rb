# frozen_string_literal: true

require 'json'

module Isureal
  module Response
    JSON_ESCAPE_PATTERN = /\\[\\bf]|[<>&\u2028\u2029]/
    JSON_CONTROL_ESCAPES = { 'b' => 8, 'f' => 12 }.freeze

    module_function

    def encode_json(value)
      JSON.generate(value).gsub(JSON_ESCAPE_PATTERN) do |match|
        next match if match == '\\\\'

        format('\\u%04x', match.start_with?('\\') ? JSON_CONTROL_ESCAPES.fetch(match[1]) : match.ord)
      end << "\n"
    end

    def format_time(time)
      time = time.getutc
      text = time.strftime('%Y-%m-%dT%H:%M:%S')
      text << ".#{format('%09d', time.nsec).sub(/0+\z/, '')}" unless time.nsec.zero?
      text << 'Z'
    end
  end

  module Log
    module_function

    def error(message, error) = write('ERROR', message, error)

    def warn(message) = write('WARN', message)

    def write(level, message, error = nil)
      entry = { time: Time.now.getutc.strftime('%Y-%m-%dT%H:%M:%S.%LZ'), level:, msg: message }
      entry[:error] = error.to_s.scrub if error
      $stdout.write(JSON.generate(entry) << "\n")
    end
  end
end
