-- A per-run secret (OTAG_RUN_TOKEN in the run's environment), stored as its SHA-256
-- hex. `otag event` and the child commands (done, fail) present it, so a hook or a
-- command naming another run is refused. NULL for runs created before tokens: they
-- accept any caller.
ALTER TABLE runs ADD COLUMN token_hash TEXT;
