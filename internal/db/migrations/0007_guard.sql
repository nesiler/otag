-- otag schema v7: guard levels (docs/ADAPTERS.md, "Threat model").
--   * projects.guard is the project setting `guard` (light | strict; light when unset).
--     A project file that already said guard = "strict" was synced into projects.config
--     as an unknown key; it keeps that level.
--   * runs.guard is the level a run started with. NULL for runs created before levels:
--     they ran with the strict rules and keep them.
ALTER TABLE projects ADD COLUMN guard TEXT NOT NULL DEFAULT 'light' CHECK (guard IN ('light', 'strict'));
UPDATE projects SET guard = 'strict' WHERE json_extract(config, '$.guard') = 'strict';
ALTER TABLE runs ADD COLUMN guard TEXT CHECK (guard IS NULL OR guard IN ('light', 'strict'));
