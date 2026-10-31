# frozen_string_literal: true

require 'openssl'

module Isureal
  module ImageSignature
    IMAGE_URL_LIFETIME = 24 * 60 * 60
    IMAGE_SIGNATURE_ITERATIONS = 1000

    module_function

    def signing_key(secret_key_hash)
      raise ArgumentError, 'invalid image signing key' unless secret_key_hash.is_a?(String) && secret_key_hash.match?(/\A\h{64}\z/)

      [secret_key_hash].pack('H*')
    end

    def signature(key, image_id, exp)
      value = "#{exp}:#{image_id}".b
      mac = OpenSSL::HMAC.new(key, 'SHA256')
      IMAGE_SIGNATURE_ITERATIONS.times { value = mac.reset.update(value).digest }
      value
    end

    def encode(signature)
      [signature].pack('m0').tr('+/', '-_').delete('=')
    end

    def decode(text)
      return nil unless text.match?(/\A[A-Za-z0-9_-]*\z/) && (text.length % 4) != 1

      decoded = (text.tr('-_', '+/') + ('=' * (-text.length % 4))).unpack1('m0')
      encode(decoded) == text ? decoded : nil
    rescue ArgumentError
      nil
    end

    def signed_url(user, image_id)
      exp = Time.now.to_i + IMAGE_URL_LIFETIME
      sig = encode(signature(user.image_key, image_id, exp))
      "/api/images/#{escape_path(image_id)}?exp=#{exp}&sig=#{sig}"
    end

    def escape_path(value)
      value.b.gsub(%r{[^A-Za-z0-9\-_.~$&+,;=:@]}) { |char| format('%%%02X', char.ord) }
    end
  end
end
