-- +goose Up
-- +goose StatementBegin

-- Extra capability axes so the router can match a task to a model's strength,
-- not just its overall intelligence tier. vision covers image understanding
-- (MMMU-Pro-class), writing covers creative/instruction-following quality
-- (EQ-Bench-class), planning covers long-horizon strategy/agentic planning
-- (GDPval/PlanBench-class). All nullable: an unmeasured axis stays NULL so the
-- scorer falls back to overall intelligence rather than reading absence as zero.
ALTER TABLE model_benchmarks ADD COLUMN vision REAL;
ALTER TABLE model_benchmarks ADD COLUMN writing REAL;
ALTER TABLE model_benchmarks ADD COLUMN planning REAL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- SQLite drops columns only via a full table rebuild; the columns are nullable
-- and ignored by older code, so leaving them in place on a downgrade is safe.

-- +goose StatementEnd
