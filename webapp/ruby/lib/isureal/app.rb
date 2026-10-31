# frozen_string_literal: true

require 'sinatra/base'

module Isureal
  HandlerContext = Data.define(:db, :config, :ip_bans)

  class App < Sinatra::Base
    set :environment, :production
    set :protection, false
    set :host_authorization, { permitted_hosts: [] }
    set :show_exceptions, false
    set :raise_errors, false
    set :dump_errors, false
    set :logging, nil
    set :x_cascade, false
    set :static, false
    set :default_content_type, nil

    def initialize(app = nil, handler_context:)
      super(app)
      @handler_context = handler_context
    end

    def call!(env)
      env[Rack::RACK_REQUEST_QUERY_HASH] = {}
      super
    end

    def db = @handler_context.db

    def config = @handler_context.config

    def ip_bans = @handler_context.ip_bans

    def json_response(code, value)
      status code
      headers['content-type'] = 'application/json'
      body Response.encode_json(value)
    end

    def blob(code, content_type, data)
      status code
      headers['content-type'] = content_type
      body data
    end

    def no_content(code)
      status code
      body ''
    end

    error Sinatra::NotFound, StandardError, Exception do |e|
      case e
      when APIError then api_error_response(e.status, e.code)
      when HTTPError then api_error_response(e.status, e.message)
      when Sinatra::NotFound then route_missing_response
      when Sinatra::BadRequest, Rack::BadRequest then api_error_response(400, "code=400, message=Bad Request, err=#{e.message}")
      else api_error_response(500, e.message)
      end
    end

    private

    def api_error_response(code, message)
      message = message.to_s.dup.force_encoding(Encoding::UTF_8).scrub
      Log.error('request failed', message) if code >= 500
      json_response(code, { error: message })
    end

    def route_missing_response
      allowed = %w[DELETE GET PATCH POST PUT].select do |method|
        method != request.request_method &&
          self.class.routes.fetch(method, []).any? { |pattern, _, _| pattern.params(request.path_info) }
      end
      return api_error_response(404, 'Not Found') if allowed.empty?

      headers['allow'] = ['OPTIONS', *allowed].join(', ')
      api_error_response(405, 'Method Not Allowed')
    end
  end
end
