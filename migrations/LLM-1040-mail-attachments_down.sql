-- Reverts LLM-1040: drops the attachments; the mail itself stays.
BEGIN;

DROP TABLE IF EXISTS mail_attachment;

COMMIT;
