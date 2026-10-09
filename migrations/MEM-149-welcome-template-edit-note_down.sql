-- MEM-149 down: restore the original save_note sentence in the default
-- welcome template.

BEGIN;

DO $MIGRATION$
DECLARE
    old_text CONSTANT text := '**Creating vs updating notes:** Use `save_note` to create a new note or fully replace an existing one. Use `edit_note` to change a specific part of a note (find-and-replace). For notes you update frequently (like `notes/active-work`), `save_note` with the full new content is usually simpler.';
    new_text CONSTANT text := '**Creating vs updating notes:** Use `save_note` only to create a new note — it fails if the slug already exists. To change an existing note, use `edit_note`: it finds an exact piece of text and replaces it. To update `notes/active-work`, replace the section that changed (for example, the whole `## Current state` section) with its new text.';
    updated integer;
BEGIN
    UPDATE templates
       SET content = replace(content, new_text, old_text),
           updated_at = NOW()
     WHERE kind = 'welcome'
       AND name = 'default'
       AND position(new_text IN content) > 0;
    GET DIAGNOSTICS updated = ROW_COUNT;
    IF updated <> 1 THEN
        RAISE EXCEPTION 'MEM-149 down: expected 1 default welcome template with the edit_note sentence, found %', updated;
    END IF;
END
$MIGRATION$;

COMMIT;
