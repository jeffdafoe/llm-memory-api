-- LLM-420: Add dream_soul_min_chars config.
-- The soul-update pass rebuilds from scratch (backloading recent dreams) only
-- when the stored soul is empty. A non-empty but truncated/degraded stub slips
-- past that check and gets "evolved" every night; because the soul-writer reads
-- its own prior output as input, the stub compounds instead of self-healing.
-- This threshold routes a suspiciously short soul (below N characters) through
-- the rebuild path too. Tune per observed healthy soul sizes; 0 disables the
-- short-stub check, leaving only the empty check.

INSERT INTO config (key, value, description) VALUES
    ('dream_soul_min_chars', '800', 'Minimum character length for a stored soul document to be treated as usable. A non-empty soul shorter than this is treated as a degraded/truncated stub and routed through the from-scratch rebuild path (backload recent dreams) instead of being evolved, so a truncated write cannot compound across dream cycles (LLM-420). Healthy souls run several KB; the observed degraded stub was 708 chars. 0 disables the short-stub check.')
ON CONFLICT (key) DO NOTHING;
