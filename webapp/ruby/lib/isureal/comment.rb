# frozen_string_literal: true

module Isureal
  COMMENT_FIELDS = %i[id post_id author_id body created_at deleted_at].freeze

  Comment = Struct.new(*COMMENT_FIELDS, :author, keyword_init: true) do
    def self.from_row(row)
      new(**row.slice(*COMMENT_FIELDS))
    end
  end

  class App
    def get_comment(db, id)
      row = db.get('SELECT id,post_id,author_id,body,created_at,deleted_at FROM comments WHERE id=$1', id)
      row && Comment.from_row(row)
    end

    def select_comments_with_author(db, conditions, *args)
      rows = db.select(<<~SQL, *args)
        SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, #{user_columns_as('u', 'author')}
        FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
        WHERE c.deleted_at IS NULL #{conditions}
      SQL
      rows.map { |row| Comment.from_row(row).tap { |comment| comment.author = User.from_row(row, 'author').set_image_key } }
    end

    def can_comment?(db, viewer_id, post_id)
      post = get_post_with_author(db, post_id)
      return false if post.nil?

      viewer = load_viewer_context(db, viewer_id)
      return false if viewer.nil?
      return false if !viewer.can_view?(post) || !post.window_ended_at.nil?
      return viewer.has_posted_today if viewer.id == post.author_id
      return true if post.visibility == 'public'

      viewer.friends.fetch(post.author_id, false)
    end
  end
end
