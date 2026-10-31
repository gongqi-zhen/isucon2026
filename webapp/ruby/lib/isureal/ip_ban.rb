# frozen_string_literal: true

module Isureal
  class IPBanList
    RELOAD_INTERVAL = 0.2

    Snapshot = Data.define(:loaded_at, :bans)

    def initialize
      @reloading = Mutex.new
      @snapshot = nil
    end

    def banned?(db, ip)
      snapshot = @snapshot
      if (snapshot.nil? || monotonic_now - snapshot.loaded_at >= RELOAD_INTERVAL) && @reloading.try_lock
        begin
          snapshot = @snapshot = load_ip_bans(db)
        ensure
          @reloading.unlock
        end
      end
      return false if snapshot.nil?

      banned_until = snapshot.bans[ip]
      !banned_until.nil? && Time.now < banned_until
    end

    private

    def load_ip_bans(db)
      rows = db.select('SELECT ip, banned_until FROM ip_bans WHERE banned_until > clock_timestamp()')
      Snapshot.new(loaded_at: monotonic_now, bans: rows.to_h { |row| [row[:ip], row[:banned_until]] })
    end

    def monotonic_now = Process.clock_gettime(Process::CLOCK_MONOTONIC)
  end

  module IPBan
    def self.extract_client_ip(env)
      ip = env['HTTP_X_CLIENT_IP']
      ip = env['REMOTE_ADDR'] if ip.nil? || ip.empty?
      (ip || '').dup.force_encoding(Encoding::UTF_8).scrub
    end
  end

  class App
    def ip_ban_middleware
      raise APIError.new(403, 'ip_banned') if ip_bans.banned?(db, real_ip)
    end

    def real_ip = IPBan.extract_client_ip(env)
  end
end
