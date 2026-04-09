-- MEM-110 rollback: Remove hosted_realms config entry

DELETE FROM config WHERE key = 'hosted_realms';
