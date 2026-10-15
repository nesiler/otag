-- otag schema v4: daemon fallback.
--   * runs.fallback_origin_profile_id is set on runs the daemon starts as a fallback: the
--     profile whose ordered fallback list the chain walks (fallbacks are not transitive).
--     NULL = a run started by a person or coordinator; it starts a new chain.
--   * ended_reason (no CHECK) now also takes stalled | quota (phase 3 daemon/hooks).
ALTER TABLE runs ADD COLUMN fallback_origin_profile_id INTEGER REFERENCES profiles (id);
