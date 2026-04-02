-- MEM-093 rollback: Remove note enrichment config.

DELETE FROM config WHERE key = 'note_enrichment_enabled';
