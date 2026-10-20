-- For databases created before the commenter was stored on orders.
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS commenter_fb_id   TEXT,
    ADD COLUMN IF NOT EXISTS commenter_name    TEXT,
    ADD COLUMN IF NOT EXISTS comment_message   TEXT,
    ADD COLUMN IF NOT EXISTS post_permalink    TEXT;
