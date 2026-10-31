# frozen_string_literal: true

require 'securerandom'

module Isureal
  class App
    def put_profile_image_handler
      raise APIError.new(415, 'unsupported_media_type') if env['CONTENT_TYPE'] != 'image/jpeg'

      data = request_body
      image = JPEG.decode(data)
      if image.nil?
        env['isureal.abuse'] = 'invalid_image'
        raise APIError.new(400, 'invalid_profile_image')
      end
      raise APIError.new(400, 'invalid_profile_image') if image.width > 512 || image.height > 512

      user_id = current_session.user_id
      db.transaction do |tx|
        row = tx.get('SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE', user_id)
        raise APIError.new(404, 'account_not_found') if row.nil?

        image_id = SecureRandom.uuid
        tx.exec("INSERT INTO image_objects(id,owner_user_id,data,media_type) VALUES ($1,$2,$3,'image/jpeg')", image_id, user_id, data)
        tx.exec('UPDATE users SET profile_image_id=$2,updated_at=clock_timestamp() WHERE id=$1', user_id, image_id)
        tx.exec('DELETE FROM image_objects WHERE id=$1', row[:profile_image_id]) unless row[:profile_image_id].nil?
      end
      no_content(204)
    end

    def delete_profile_image_handler
      user_id = current_session.user_id
      db.transaction do |tx|
        row = tx.get('SELECT profile_image_id FROM users WHERE id=$1 FOR NO KEY UPDATE', user_id)
        raise APIError.new(404, 'account_not_found') if row.nil? || row[:profile_image_id].nil?

        tx.exec('UPDATE users SET profile_image_id=NULL,updated_at=clock_timestamp() WHERE id=$1', user_id)
        tx.exec('DELETE FROM image_objects WHERE id=$1', row[:profile_image_id])
      end
      no_content(204)
    end
  end
end
