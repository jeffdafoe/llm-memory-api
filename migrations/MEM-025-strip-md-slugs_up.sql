-- MEM-025: Strip .md extension from all document slugs.
-- Slugs are logical identifiers, not filenames. The .md extension was a
-- migration artifact from when notes were stored as files. titleToSlug()
-- already generates clean slugs, so existing .md slugs would cause
-- divergence over time.

-- 1. Strip .md from document slugs
UPDATE documents
SET slug = REGEXP_REPLACE(slug, '\.md$', '')
WHERE slug LIKE '%.md';

-- 2. Strip .md from vector store source_file (mirrors document slugs)
UPDATE memories
SET source_file = REGEXP_REPLACE(source_file, '\.md$', '')
WHERE source_file LIKE '%.md';

-- 3. Fix slug references inside bootstrap note content.
-- These contain patterns like slug="instructions/bootstrap.md" that need
-- the .md stripped. Only update documents that actually contain the pattern.
UPDATE documents
SET content = REPLACE(content, '.md")', '")')
WHERE content LIKE '%slug=%.md"%';

UPDATE documents
SET content = REPLACE(content, '.md"', '"')
WHERE content LIKE '%slug=%.md"%';
