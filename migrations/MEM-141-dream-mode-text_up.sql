-- MEM-141 — convert agent_configuration.dream_mode from the dream_mode_t enum to
-- plain TEXT, so the 'sim-shared' mode (LLM-515, Slice 1) is just another string
-- value with no schema ceremony.
--
-- Why drop the enum: ALTER TYPE ... ADD VALUE can't be used in the same
-- transaction it's added, and an enum value can never be removed without
-- recreating the whole type. The project already moved off enums for this kind
-- of field (dream_source is TEXT, MEM-137); dream_mode predated that and isn't
-- relied on as an enum anywhere, so we convert it to TEXT. Allowed values are
-- enforced app-side (routes/admin.js, registration.js), not by a DB constraint.
--
-- 'sim-shared' marks a pooled shared-VA agent (salem-vendor) whose per-actor day
-- material is distilled into per-actor notes under each actor's slug-prefix,
-- WITHOUT the pool ever being dreamed as one collapsed identity: the daily
-- conversation-day push accepts it (writing under the prefix), while the
-- per-agent dream loop (dream_mode IN ('companion','technical','sim'))
-- deliberately excludes it.
--
-- The agent_status view SELECTs agc.dream_mode, so PostgreSQL blocks altering the
-- column's type while the view exists. Drop it, convert the column, then recreate
-- it verbatim — the view only passes dream_mode through, so the recreated
-- definition is identical and only the underlying column type changes. The whole
-- migration runs in one transaction so a mid-way failure can't leave the view
-- dropped.

BEGIN;

DROP VIEW IF EXISTS agent_status;

ALTER TABLE agent_configuration ALTER COLUMN dream_mode DROP DEFAULT;
ALTER TABLE agent_configuration
    ALTER COLUMN dream_mode TYPE TEXT USING dream_mode::text;
ALTER TABLE agent_configuration ALTER COLUMN dream_mode SET DEFAULT 'none';

DROP TYPE dream_mode_t;

CREATE VIEW agent_status AS
 SELECT ac.id AS actor_id,
    ac.name AS agent,
        CASE
            WHEN agc.virtual = true AND (ac.status::text = ANY (ARRAY['available'::character varying, 'degraded'::character varying, 'error'::character varying]::text[])) THEN ac.status
            WHEN agc.virtual = true THEN 'available'::character varying
            WHEN ac.last_seen > (now() - '00:15:00'::interval) THEN 'online'::character varying
            WHEN ac.last_seen IS NOT NULL THEN 'offline'::character varying
            ELSE 'unknown'::character varying
        END AS status,
    ac.last_seen,
    ac.passphrase_rotated_at,
    ac.created_at AS registered_at,
    ac.expertise,
    agc.provider,
    agc.model,
    agc.virtual,
    agc.personality,
    agc.cost_budget_daily,
    agc.cost_budget_monthly,
        CASE
            WHEN ac.active_since IS NOT NULL AND ac.active_since > (now() - '00:30:00'::interval) THEN ac.active_since
            ELSE NULL::timestamp with time zone
        END AS active_since,
    agc.cache_prompts,
    agc.learning_enabled,
    agc.max_tokens,
    agc.temperature,
    agc.dream_mode,
    agc.storage_quota
   FROM actors ac
     JOIN agent_configuration agc ON agc.actor_id = ac.id;

-- Recreating the view reset its grants to the executor (postgres, still the
-- owner). Restore the grants the original view carried. The app user (memory_api)
-- is also re-granted by the deploy's post-migration grant step; restoring it here
-- keeps the migration self-contained. Guarded on role existence so a fresh DB
-- lacking a role can't fail the migration.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'memory_api') THEN
        GRANT ALL ON agent_status TO memory_api;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'claude') THEN
        GRANT INSERT, SELECT, UPDATE, DELETE ON agent_status TO claude;
    END IF;
END $$;

COMMIT;
