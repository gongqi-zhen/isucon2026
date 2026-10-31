# frozen_string_literal: true

require 'stringio'
require 'zip'

module Isureal
  class App
    def export_posts_handler
      user_id = current_session.user_id
      author = get_user(db, user_id) or raise NoRowsError
      posts = db.select(<<~SQL, user_id)
        SELECT #{POST_COLUMNS}, i.data AS image_data
        FROM posts p
        JOIN posting_windows w ON w.id = p.posting_window_id
        JOIN image_objects i ON i.id = p.image_id
        WHERE p.author_id = $1 AND p.deleted_at IS NULL
        ORDER BY p.posted_at, p.id
      SQL

      archive = Zip::OutputStream.write_buffer(StringIO.new) do |writer|
        items = posts.each_with_index.map do |row, index|
          image_path = format('images/%06d.jpg', index + 1)
          writer.put_next_entry(image_path, '', Zip::ExtraField.new, Zip::Entry::STORED)
          writer.write(row[:image_data])
          post_response(Post.from_row(row), author).merge(imagePath: image_path)
        end
        writer.put_next_entry('posts.json')
        writer.write(Response.encode_json(items))
      end
      headers['content-disposition'] = 'attachment; filename="isureal-posts.zip"'
      headers['cache-control'] = 'no-store'
      blob(200, 'application/zip', archive.string)
    end
  end
end
