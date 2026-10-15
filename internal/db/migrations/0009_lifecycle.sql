-- otag schema v9: phase 4 lifecycle (docs/DAEMON.md, "Park and resume"; docs/DESIGN.md).
--   * A project has at most one coordinator run that has not ended (`otag coord`).
--   * v_model_usage: token usage per reported model (run_usage.model; the profile's
--     configured model when a harness reported none), for `otag stats`.
CREATE UNIQUE INDEX runs_one_live_coordinator ON runs (project_id) WHERE role = 'coordinator' AND status <> 'ended';

CREATE VIEW v_model_usage AS
SELECT coalesce(nullif(u.model, ''), nullif(pv.model, ''), '?') AS model,
       r.harness,
       count(DISTINCT u.run_id)     AS runs,
       sum(u.input_tokens)          AS input_tokens,
       sum(u.output_tokens)         AS output_tokens,
       sum(u.cache_read_tokens)     AS cache_read_tokens,
       sum(u.cache_write_tokens)    AS cache_write_tokens,
       sum(u.reasoning_tokens)      AS reasoning_tokens,
       sum(u.cost_usd)              AS cost_usd,
       max(u.estimated)             AS estimated
FROM run_usage u
JOIN runs r ON r.id = u.run_id
JOIN profile_revisions pv ON pv.id = r.profile_revision_id
GROUP BY 1, 2;
