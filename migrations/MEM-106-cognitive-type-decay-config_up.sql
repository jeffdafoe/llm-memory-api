-- MEM-106: Add config keys for cognitive type decay half-lives.
-- Cognitive type (semantic/episodic/procedural/reflective) is assigned by
-- enrichment and stored in metadata. When present, these decay rates
-- override the per-kind decay rates in search scoring.

INSERT INTO config (key, value, description) VALUES
    ('search_decay_halflife_semantic', '0', 'Decay half-life (days) for semantic memories (facts, definitions). 0 = no decay.'),
    ('search_decay_halflife_episodic', '90', 'Decay half-life (days) for episodic memories (events). 0 = no decay.'),
    ('search_decay_halflife_procedural', '0', 'Decay half-life (days) for procedural memories (decisions, conventions). 0 = no decay.'),
    ('search_decay_halflife_reflective', '180', 'Decay half-life (days) for reflective memories (insights, analysis). 0 = no decay.')
ON CONFLICT (key) DO NOTHING;
