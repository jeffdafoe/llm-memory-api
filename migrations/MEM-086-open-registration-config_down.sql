-- MEM-086 down: Remove open_registration config key
DELETE FROM config WHERE key = 'open_registration';
