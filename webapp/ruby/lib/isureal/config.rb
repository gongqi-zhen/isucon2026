# frozen_string_literal: true

module Isureal
  Config = Struct.new(:region, :db_host, :db_port, :db_user, :db_password, :db_name, :log_level, keyword_init: true) do
    def self.from_env
      new(
        region: env_or_default('ISUREAL_REGION', 'asia'),
        db_host: env_or_default('ISUREAL_DB_HOST', '127.0.0.1'),
        db_port: env_or_default('ISUREAL_DB_PORT', '5432'),
        db_user: env_or_default('ISUREAL_DB_USER', 'isureal'),
        db_password: env_or_default('ISUREAL_DB_PASSWORD', 'isureal'),
        db_name: env_or_default('ISUREAL_DB_NAME', 'isureal'),
        log_level: env_or_default('ISUREAL_LOG_LEVEL', 'debug'),
      )
    end

    def self.env_or_default(key, default_value)
      value = ENV[key]
      value.nil? || value.empty? ? default_value : value
    end

    def database_options
      { host: db_host, port: db_port, user: db_user, password: db_password, dbname: db_name }
    end
  end
end
