-- MEM-094 rollback: Remove conversation chunk config keys.

DELETE FROM config WHERE key IN ('conversation_chunk_window', 'conversation_chunk_overlap', 'conversation_chunk_max_chars');
