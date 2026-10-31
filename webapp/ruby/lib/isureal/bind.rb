# frozen_string_literal: true

require 'date'
require 'json'

module Isureal
  module Bind
    FLOAT_PATTERN = /\A[+-]?(?:(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?|inf(?:inity)?|nan)\z/i
    INT_PATTERN = /\A[+-]?\d+\z/
    RFC3339_PATTERN = /\A(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:[.,](\d+))?(?:(Z)|([+-])(\d{2}):(\d{2}))\z/
    QUERY_METHODS = %w[GET DELETE HEAD].freeze

    module_function

    def parse_query(raw)
      values = {}
      error = nil
      raw.to_s.split('&').each do |pair|
        if pair.include?(';')
          error ||= 'invalid semicolon separator in query'
          next
        end
        next if pair.empty?

        key, value = pair.split('=', 2)
        begin
          key = query_unescape(key)
          value = query_unescape(value || '')
        rescue ArgumentError => e
          error ||= e.message
          next
        end
        (values[key] ||= []) << value
      end
      [values, error]
    end

    def query_unescape(value)
      value = value.b
      if (invalid = value.match(/%(?![0-9A-Fa-f]{2})/))
        raise ArgumentError, "invalid URL escape #{value[invalid.begin(0), 3].inspect}"
      end

      value.tr('+', ' ').gsub(/%(\h\h)/) { ::Regexp.last_match(1).hex.chr }.force_encoding(Encoding::UTF_8)
    end

    def parse_time(value)
      match = RFC3339_PATTERN.match(value)
      raise ArgumentError, "parsing time #{value.inspect} as \"2006-01-02T15:04:05Z07:00\"" unless match

      year, month, day, hour, minute, second = match.captures.first(6).map(&:to_i)
      unless Date.valid_date?(year, month, day) && hour <= 23 && minute <= 59 && second <= 59
        raise ArgumentError, "parsing time #{value.inspect}: value out of range"
      end

      fraction = Rational((match[7] || '0')[0, 9].ljust(9, '0').to_i, 1_000_000_000)
      time = Time.utc(year, month, day, hour, minute, second + fraction)
      return time if match[8]

      offset_hour = match[10].to_i
      offset_minute = match[11].to_i
      raise ArgumentError, "parsing time #{value.inspect}: time zone offset out of range" if offset_hour > 23 || offset_minute > 59

      offset = ((offset_hour * 60) + offset_minute) * 60
      match[9] == '-' ? time + offset : time - offset
    end

    def parse_float(value)
      return 0.0 if value.empty?
      raise ArgumentError, "strconv.ParseFloat: parsing #{value.inspect}: invalid syntax" unless FLOAT_PATTERN.match?(value)

      body = value.delete_prefix('+').delete_prefix('-')
      sign = value.start_with?('-') ? -1 : 1
      return Float::NAN if body.casecmp?('nan')
      return sign * Float::INFINITY if body.downcase.start_with?('inf')

      result = Float(value.start_with?('.', '+.', '-.') ? value.sub('.', '0.') : value.sub(/\.(?=[eE]|\z)/, ''))
      raise ArgumentError, "strconv.ParseFloat: parsing #{value.inspect}: value out of range" if result.infinite?

      result
    end

    def parse_int(value)
      return 0 if value.empty?
      raise ArgumentError, "strconv.ParseInt: parsing #{value.inspect}: invalid syntax" unless INT_PATTERN.match?(value)

      result = Integer(value, 10)
      raise ArgumentError, "strconv.ParseInt: parsing #{value.inspect}: value out of range" unless result.bit_length < 64

      result
    end

    def convert(value, type)
      case type
      when :time then parse_time(value)
      when :float then parse_float(value)
      when :int then parse_int(value)
      else value
      end
    end

    def convert_json(value, type)
      case type
      when :string, :string_pointer
        raise ArgumentError, "cannot unmarshal #{json_type(value)} into string" unless value.is_a?(String)

        value.scrub
      when :int
        unless value.is_a?(Integer) && value.bit_length < 64
          raise ArgumentError, "cannot unmarshal #{json_type(value)} into int64"
        end

        value
      else
        raise ArgumentError, "cannot unmarshal #{json_type(value)} into #{type}"
      end
    end

    def json_type(value)
      case value
      when String then 'string'
      when Numeric then "number #{value}"
      when true, false then 'bool'
      when Array then 'array'
      when Hash then 'object'
      else 'null'
      end
    end

    def lookup(values, name)
      values.fetch(name) { values.find { |key, _| key.casecmp?(name) }&.last }
    end
  end

  class App
    def bind(fields)
      input = fields.to_h { |name, field| [name, field.fetch(:type, :string) == :string ? +'' : nil] }
      bind_fields(input, fields, :param) { |name| params[name] && [params[name]] }
      if Bind::QUERY_METHODS.include?(request.request_method)
        values = query_values
        bind_fields(input, fields, :query) { |name| Bind.lookup(values, name) }
      end
      bind_body(input, fields)
      input
    end

    def query_values
      @query_values ||= Bind.parse_query(request.query_string).first
    end

    def media_type
      (env['CONTENT_TYPE'] || '').split(';', 2).first.to_s.strip
    end

    def request_body
      @request_body ||= begin
        input = env['rack.input']
        input.rewind if input.respond_to?(:rewind)
        (input&.read || '').b
      end
    end

    private

    def bind_fields(input, fields, tag)
      fields.each do |name, field|
        key = field[tag] or next
        values = yield(key) or next
        value = values.first
        next if value.nil?

        begin
          input[name] = Bind.convert(value, field.fetch(:type, :string))
        rescue ArgumentError => e
          raise HTTPError.bad_request("#{key}: #{e.message}")
        end
      end
    end

    def bind_body(input, fields)
      return if request_body.empty?

      case media_type
      when 'application/json' then bind_json(input, fields)
      when 'application/xml', 'text/xml' then nil
      when 'application/x-www-form-urlencoded' then bind_form(input, fields, urlencoded_form_values)
      when 'multipart/form-data' then bind_multipart(input, fields)
      else raise HTTPError.unsupported_media_type
      end
    end

    def bind_json(input, fields)
      data = begin
        JSON.parse(request_body.dup.force_encoding(Encoding::UTF_8).scrub, allow_duplicate_key: true)
      rescue JSON::ParserError => e
        raise HTTPError.bad_request(e.message)
      end
      return if data.nil?
      raise HTTPError.bad_request("cannot unmarshal #{Bind.json_type(data)} into request") unless data.is_a?(Hash)

      fields.each do |name, field|
        key = field[:json] or next
        value = data[key]
        next if value.nil?

        begin
          input[name] = Bind.convert_json(value, field.fetch(:type, :string))
        rescue ArgumentError => e
          raise HTTPError.bad_request("#{key}: #{e.message}")
        end
      end
    end

    def urlencoded_form_values
      body_values, error = Bind.parse_query(request_body)
      raise HTTPError.bad_request(error) if error

      Bind.parse_query(request.query_string).first.each { |key, values| (body_values[key] ||= []).concat(values) }
      body_values
    end

    def bind_form(input, fields, values)
      bind_fields(input, fields, :form) { |name| Bind.lookup(values, name) }
    end

    def bind_multipart(input, fields)
      values = {}
      files = {}
      request.form_pairs.each do |key, value|
        if value.is_a?(Hash)
          (files[key] ||= []) << value[:tempfile] if value[:tempfile]
        else
          (values[key] ||= []) << value.to_s.dup.force_encoding(Encoding::UTF_8)
        end
      end
      file_fields, value_fields = fields.partition { |_, field| field[:form] && field[:type] == :file }
      file_fields.each { |name, field| input[name] = files[field[:form]]&.first }
      bind_form(input, value_fields.to_h, values)
    end
  end
end
