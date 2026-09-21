-- +goose Up
-- +goose StatementBegin

-- Honest per-request usage and cost accounting. The trail already carried
-- token counts and an `estimated` flag, but two states were being conflated:
-- a request whose provider omitted usage entirely (genuinely unknown) read the
-- same as one that used zero tokens, and a model with no published price read
-- as free rather than unknown. These columns make the difference durable.
--
-- usage_quality is the three-state honesty marker the analytics surface reads:
--   'exact'       provider-reported token counts
--   'estimated'   counts we derived because the provider omitted usage
--   'unavailable' no usage at all -- a failed hop, or a provider that reported
--                 nothing that we could not estimate.
-- cost_known separates a computed or provider-reported cost from an unknown
-- one (no published price). When it is 0, cost_usd carries no meaning and the
-- API renders the cost as unknown, never as free.
ALTER TABLE requests ADD COLUMN usage_quality TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN cost_known INTEGER NOT NULL DEFAULT 0;

-- Backfill the trail. An already-estimated row keeps that marker; a zero-token
-- row that was never estimated is unavailable, not exact; everything else had
-- real reported counts.
UPDATE requests SET usage_quality = 'estimated' WHERE estimated = 1;
UPDATE requests SET usage_quality = 'unavailable'
 WHERE estimated = 0 AND input_tokens = 0 AND output_tokens = 0;
UPDATE requests SET usage_quality = 'exact'
 WHERE estimated = 0 AND (input_tokens > 0 OR output_tokens > 0);

-- Historical cost was written as a bare 0 for every row, which read as "free".
-- Only a row that actually carried a positive cost had a known price; the rest
-- become unknown-cost, matching the new write path.
UPDATE requests SET cost_known = 1 WHERE cost_usd > 0;

-- The hourly rollup gains the same honesty so a pruned trail still yields an
-- honest chart. These counts start at 0 and accumulate from the next request:
-- the historical breakdown is not reconstructable from the rollup alone, and
-- re-deriving it from a partially pruned trail would be worse than absent.
ALTER TABLE request_hourly ADD COLUMN estimated_requests INTEGER NOT NULL DEFAULT 0;
ALTER TABLE request_hourly ADD COLUMN unavailable_requests INTEGER NOT NULL DEFAULT 0;
ALTER TABLE request_hourly ADD COLUMN cost_known_requests INTEGER NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE request_hourly DROP COLUMN cost_known_requests;
ALTER TABLE request_hourly DROP COLUMN unavailable_requests;
ALTER TABLE request_hourly DROP COLUMN estimated_requests;
ALTER TABLE requests DROP COLUMN cost_known;
ALTER TABLE requests DROP COLUMN usage_quality;
-- +goose StatementEnd
