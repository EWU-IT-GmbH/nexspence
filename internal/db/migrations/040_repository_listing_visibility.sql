-- +goose Up
ALTER TABLE repositories ADD COLUMN hide_from_anonymous_lists BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE repositories DROP COLUMN hide_from_anonymous_lists;
