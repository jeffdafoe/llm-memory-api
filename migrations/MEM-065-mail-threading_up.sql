-- MEM-065: Mail threading and selective receive.
-- Adds in_reply_to column for threading replies to original messages.

ALTER TABLE mail ADD COLUMN in_reply_to UUID REFERENCES mail(id);
