-- MEM-106 down: Remove cognitive type decay config keys.

DELETE FROM config WHERE key IN (
    'search_decay_halflife_semantic',
    'search_decay_halflife_episodic',
    'search_decay_halflife_procedural',
    'search_decay_halflife_reflective'
);
