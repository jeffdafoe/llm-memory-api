-- MEM-071 down: Remove global_bootstrap config key

DELETE FROM config WHERE key = 'global_bootstrap';
