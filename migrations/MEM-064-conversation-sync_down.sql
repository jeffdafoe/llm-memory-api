-- MEM-064 rollback: Remove conversation sync support.

ALTER TABLE documents DROP COLUMN IF EXISTS metadata;

UPDATE documents SET kind = 'note' WHERE kind = 'conversation';

DELETE FROM config WHERE key IN (
    'search_decay_halflife_conversation',
    'search_conversation_weight',
    'conversation_retention_days'
);
