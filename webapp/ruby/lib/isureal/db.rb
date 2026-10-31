# frozen_string_literal: true

require 'connection_pool'
require 'date'
require 'pg'

module Isureal
  class Database
    def initialize(options, size:, timeout: 60)
      PG.connect(**options).close
      @pool = ConnectionPool.new(size:, timeout:) { Connection.new(PG.connect(**options)) }
    end

    def with
      @pool.with do |conn|
        yield conn
      rescue PG::ConnectionBad, PG::UnableToSend
        @pool.discard_current_connection(&:close)
        raise
      end
    end

    def get(...) = with { |conn| conn.get(...) }

    def get!(...) = with { |conn| conn.get!(...) }

    def get_value(...) = with { |conn| conn.get_value(...) }

    def get_value!(...) = with { |conn| conn.get_value!(...) }

    def select(...) = with { |conn| conn.select(...) }

    def select_values(...) = with { |conn| conn.select_values(...) }

    def exec(...) = with { |conn| conn.exec(...) }

    def transaction(&) = with { |conn| conn.transaction(&) }
  end

  class Connection
    STATEMENT_CACHE_CAPACITY = 512
    TYPE_REGISTRY = PG::BasicTypeRegistry.new.register_default_types
                                         .register_type(0, 'numeric', PG::TextEncoder::Float, PG::TextDecoder::Float)

    def initialize(raw)
      @raw = raw
      @raw.type_map_for_results = PG::BasicTypeMapForResults.new(@raw, registry: TYPE_REGISTRY)
      @raw.field_name_type = :symbol
      @statements = {}
      @statement_count = 0
    end

    def get(sql, *params) = query(sql, params).first

    def get!(sql, *params) = get(sql, *params) || raise(NoRowsError)

    def get_value(sql, *params)
      result = query(sql, params)
      result.ntuples.zero? ? nil : result.getvalue(0, 0)
    end

    def get_value!(sql, *params)
      result = query(sql, params)
      raise NoRowsError if result.ntuples.zero?

      result.getvalue(0, 0)
    end

    def select(sql, *params) = query(sql, params).to_a

    def select_values(sql, *params) = query(sql, params).column_values(0)

    def exec(sql, *params) = query(sql, params).cmd_tuples

    def transaction
      @raw.transaction { yield self }
    end

    def close
      @raw.close
    rescue PG::Error
      nil
    end

    private

    def query(sql, params)
      @raw.exec_prepared(statement(sql), params.map { |param| encode(param) })
    end

    def statement(sql)
      if (name = @statements.delete(sql))
        return @statements[sql] = name
      end

      if @statements.size >= STATEMENT_CACHE_CAPACITY
        _, oldest = @statements.shift
        @raw.exec("DEALLOCATE #{oldest}")
      end
      name = "isureal_#{@statement_count += 1}"
      @raw.prepare(name, sql)
      @statements[sql] = name
    end

    def encode(value)
      case value
      when nil then nil
      when String then value.encoding == Encoding::BINARY ? { value:, format: 1 } : value
      when Time then value.getutc.strftime('%Y-%m-%d %H:%M:%S.%6N+00')
      when Date then value.iso8601
      else value.to_s
      end
    end
  end
end
