-- +goose Up
-- +goose StatementBegin

-- Durable record of models an operator removed from a profile. profile_models
-- has no per-row enable flag: membership IS the presence of a row, so a removal
-- is a DELETE, and the Default profile's boot-time auto-include re-added exactly
-- those deletions on every restart. This table remembers them so a removal
-- sticks. It is maintained by every membership mutation -- a removal inserts a
-- row here, an explicit re-add deletes it -- and EnsureDefaultProfile appends
-- only enabled models that are neither members nor excluded.
CREATE TABLE IF NOT EXISTS profile_exclusions (
    profile_id  INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    model_db_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    PRIMARY KEY (profile_id, model_db_id)
);

-- Additive backfill for already-created state. Prior boots recorded no
-- exclusions, so an install upgrading to this schema would resurrect every
-- Default-profile removal on its next boot. At steady state the Default profile
-- auto-includes every enabled model, so any enabled model currently ABSENT from
-- it was removed by the operator: capture that difference as durable
-- exclusions. A fresh install has no Default profile yet (it is created at boot,
-- after migrations run) and its models table is still empty here, so this is a
-- no-op there and no genuinely-new model is ever wrongly excluded.
INSERT OR IGNORE INTO profile_exclusions (profile_id, model_db_id)
SELECT p.id, m.id
  FROM profiles p
  JOIN models m ON m.enabled = 1
 WHERE LOWER(p.name) = 'default'
   AND m.id NOT IN (SELECT model_db_id FROM profile_models WHERE profile_id = p.id);

-- Case-insensitive profile-name uniqueness at the constraint level, so "Coding"
-- and "coding" can never coexist however the insert path is spelled. The table
-- kept only a case-SENSITIVE UNIQUE(name) until now, so already-created state
-- may hold case-collisions; disambiguate them first by keeping the earliest
-- row's name and suffixing the rest with their unique id. Every profile and its
-- membership is preserved -- only the later duplicates' names change -- so the
-- index below can be built without failing on existing data.
UPDATE profiles
   SET name = name || ' (' || id || ')'
 WHERE id NOT IN (SELECT MIN(id) FROM profiles GROUP BY LOWER(name))
   AND LOWER(name) IN (
       SELECT LOWER(name) FROM profiles GROUP BY LOWER(name) HAVING COUNT(*) > 1
   );

CREATE UNIQUE INDEX IF NOT EXISTS idx_profiles_name_nocase ON profiles(LOWER(name));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_profiles_name_nocase;
DROP TABLE IF EXISTS profile_exclusions;
-- +goose StatementEnd
