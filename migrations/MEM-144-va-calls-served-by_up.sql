-- MEM-144 — record which upstream host served each OpenRouter call (LLM-560).
--
-- virtual_agent_calls.cost used to be an estimate priced from OpenRouter's
-- /api/v1/models catalog: one listed price per model id. But OpenRouter fronts
-- many upstream hosts per model at differing prices, and LLM-328 pins the sim
-- agents to one of them (Baidu). So the listed price and the billed price can
-- diverge, and on 2026-07-26 they did — the deepseek-v4-flash listing stepped
-- ~$0.096 -> $0.14/Mtok between 00:00 and 01:00 UTC, logged village spend
-- appeared to jump ~50% overnight, and none of it was real. The pinned traffic
-- was still being billed at Baidu's rate the whole time. Establishing that cost
-- three days later took live test calls and the credits balance, because our
-- own DB could not answer it.
--
-- The code half of the fix takes the billed cost off the response (which has
-- always carried it) instead of pricing the call ourselves. This column is the
-- other half: without knowing WHICH host served a call, a per-call cost is a
-- number with no explanation, and the next routing or pricing shift is again
-- undiagnosable from the DB. With it, a daily cost query can separate
-- pinned-upstream traffic from traffic that fell back elsewhere.
--
-- NULL for every provider we call directly (Anthropic, OpenAI, Google, xAI,
-- Perplexity) — they serve their own models, so there is no distinct upstream
-- to name. NULL also on rows written before this migration, and on failed calls
-- where no response body was parsed. TEXT rather than a constrained type
-- because the value is OpenRouter's free-form display name for the host
-- ("Baidu", "DeepInfra", "Together"), and their roster changes without notice.

ALTER TABLE virtual_agent_calls ADD COLUMN IF NOT EXISTS served_by TEXT;

COMMENT ON COLUMN virtual_agent_calls.served_by IS
    'OpenRouter upstream host that served this call (response .provider). NULL for direct providers and pre-LLM-560 rows.';
