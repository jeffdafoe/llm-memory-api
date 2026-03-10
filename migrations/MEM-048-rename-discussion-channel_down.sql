-- MEM-048 rollback: Revert channel prefix from 'discussion-' back to 'discuss-'.

UPDATE chat_messages
SET channel = 'discuss-' || SUBSTRING(channel FROM 12)
WHERE channel LIKE 'discussion-%';

UPDATE discussions
SET channel = 'discuss-' || SUBSTRING(channel FROM 12)
WHERE channel LIKE 'discussion-%';
