# frozen_string_literal: true

# 2026-09-23

require_relative 'lib/isureal'

$stdout.sync = true

run Isureal.new_server(Isureal::Config.from_env, pool_size: Integer(ENV.fetch('ISUREAL_DB_POOL_SIZE', '15')))
