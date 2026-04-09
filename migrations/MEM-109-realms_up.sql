-- MEM-109: Add realm-based scoping to actors and invite codes
--
-- Realms provide lightweight multi-tenancy. Agents only see other agents
-- that share at least one realm. Host-to-realm mapping in config determines
-- the default realm for new registrations.

BEGIN;

-- 1. Add realms array to actors (defaults to empty — will backfill below)
ALTER TABLE actors ADD COLUMN realms text[] NOT NULL DEFAULT '{}';

-- 2. Backfill existing agents
UPDATE actors SET realms = '{llm-memory}' WHERE name IN (
    'home', 'work', 'jeff', 'wendy', 'dave', 'smith',
    'code_review', 'design_review', 'designer',
    'openai-general', 'claude-general', 'home-chatgpt',
    'search-general', 'search-twitter-x',
    'memory-enrichment', 'actor-name-check',
    'dream-companion', 'dream-companion-soul',
    'dream-technical', 'dream-technical-soul',
    'test-gemini'
);

UPDATE actors SET realms = '{zbbs}' WHERE name IN (
    'zbbs-ezekiel-crane', 'zbbs-josiah-thorne', 'zbbs-prudence-ward'
);

-- Jeff sees everything — add both realms
UPDATE actors SET realms = '{llm-memory,zbbs}' WHERE name = 'jeff';
-- Wendy too for now
UPDATE actors SET realms = '{llm-memory,zbbs}' WHERE name = 'wendy';

-- Catch any actors not yet assigned (e.g. future agents created before this migration)
UPDATE actors SET realms = '{llm-memory}' WHERE realms = '{}';

-- 3. Add realm to invite codes so new signups inherit it
ALTER TABLE invite_codes ADD COLUMN realm text NOT NULL DEFAULT 'llm-memory';

-- 4. Add host-to-realm mapping in config
INSERT INTO config (key, value, description)
VALUES ('realm_host_map', '{"llm-memory.net":"llm-memory","village.llm-memory.net":"zbbs"}',
        'JSON object mapping request hostnames to realm names for registration and UI scoping');

-- 5. Index for realm queries
CREATE INDEX idx_actors_realms ON actors USING GIN (realms);

COMMIT;
