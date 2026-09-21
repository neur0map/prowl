-- +goose Up
-- +goose StatementBegin

-- Provider availability for a model, distinct from the operator's own `enabled`
-- toggle. A subscription login is reconciled on every boot and on demand: a
-- model the provider stops offering must stop being a routing candidate, but the
-- operator's own decision to disable it must survive the provider dropping and
-- later re-offering it. Overloading `enabled` for both conflated them --
-- reconciliation re-enabled a model the operator had turned off -- so provider
-- availability gets its own column. Retirement clears `available`;
-- reconciliation restores it; neither ever touches `enabled`. Every routing,
-- catalog and provider-status candidate query requires BOTH flags. Catalog,
-- custom and discovered rows are always available, so the default is 1 and this
-- additive backfill leaves every existing row routable.
ALTER TABLE models ADD COLUMN available INTEGER NOT NULL DEFAULT 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE models DROP COLUMN available;
-- +goose StatementEnd
