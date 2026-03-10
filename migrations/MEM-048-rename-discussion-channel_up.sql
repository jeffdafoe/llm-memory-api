-- MEM-048: Rename discussion channel prefix from 'discuss-' to 'discussion-'.
-- Aligns channel naming with tool naming convention (discussion_create, discussion_*).

UPDATE chat_messages
SET channel = 'discussion-' || SUBSTRING(channel FROM 9)
WHERE channel LIKE 'discuss-%'
  AND channel NOT LIKE 'discussion-%';

UPDATE discussions
SET channel = 'discussion-' || SUBSTRING(channel FROM 9)
WHERE channel LIKE 'discuss-%'
  AND channel NOT LIKE 'discussion-%';
