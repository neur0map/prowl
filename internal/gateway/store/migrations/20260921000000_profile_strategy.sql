-- +goose Up
-- +goose StatementBegin

-- A set's routing strategy is part of what the set means, not a global knob:
-- "quick chores" wants fastest-first, "deep work" wants smartest-first, and the
-- operator should not have to flip one global strategy every time they switch
-- sets. This column carries the strategy the set orders its own chain by. An
-- empty string means "inherit the operator's default" (settings.routing_strategy,
-- balanced when unset), so this additive backfill leaves every existing set
-- behaving exactly as before.
ALTER TABLE profiles ADD COLUMN strategy TEXT NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE profiles DROP COLUMN strategy;
-- +goose StatementEnd
