-- MEM-118: Remove dream interchunk delay config key.

DELETE FROM config WHERE key = 'dream_interchunk_delay';
