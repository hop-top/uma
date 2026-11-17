-- A schema version newer than the embedded migrations, as written by a
-- later release of this package.

-- +goose Up
CREATE TABLE uma_newer_schema_marker (id integer PRIMARY KEY);

-- +goose Down
DROP TABLE uma_newer_schema_marker;
