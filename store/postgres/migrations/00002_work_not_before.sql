-- Deferred work: an item with not_before set is not claimable before that
-- time, judged by the store's clock.

-- +goose Up

ALTER TABLE uma_work ADD COLUMN not_before timestamptz;

-- +goose Down

ALTER TABLE uma_work DROP COLUMN not_before;
