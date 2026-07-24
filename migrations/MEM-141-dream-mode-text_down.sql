-- MEM-141 down — restore the dream_mode_t enum and convert the column back.
--
-- Assumes MEM-142 down has already run (it resets any 'sim-shared' rows to
-- 'none', a value the restored enum lacks). Same view drop/recreate dance as the
-- up, in reverse, in one transaction.

BEGIN;

DROP VIEW agent_status;

CREATE TYPE dream_mode_t AS ENUM ('none', 'companion', 'technical', 'sim');

ALTER TABLE agent_configuration ALTER COLUMN dream_mode DROP DEFAULT;
ALTER TABLE agent_configuration
    ALTER COLUMN dream_mode TYPE dream_mode_t USING dream_mode::dream_mode_t;
ALTER TABLE agent_configuration ALTER COLUMN dream_mode SET DEFAULT 'none';

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

COMMIT;
