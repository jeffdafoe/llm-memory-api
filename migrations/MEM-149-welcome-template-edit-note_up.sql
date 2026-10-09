-- MEM-149: the default welcome template told agents to update notes with
-- save_note ("save_note with the full new content is usually simpler"), but
-- save_note over MCP is insert-only and fails when the slug exists. Point
-- updates at edit_note instead (LLM-729).
--
-- The live template was last edited by hand in the admin UI, so this patches
-- the one sentence in place rather than rewriting the whole template, and
-- fails if the sentence is not there exactly.
--
-- Signup copies the template into agent_configuration.startup_instructions,
-- so existing accounts carry the same sentence. Those copies are patched too:
-- only the exact sentence is replaced, so anything an account changed in its
-- own instructions is kept. Accounts whose copy no longer has the sentence
-- word for word are left alone.

BEGIN;

DO $MIGRATION$
DECLARE
    old_text CONSTANT text := '**Creating vs updating notes:** Use `save_note` to create a new note or fully replace an existing one. Use `edit_note` to change a specific part of a note (find-and-replace). For notes you update frequently (like `notes/active-work`), `save_note` with the full new content is usually simpler.';
    new_text CONSTANT text := '**Creating vs updating notes:** Use `save_note` only to create a new note — it fails if the slug already exists. To change an existing note, use `edit_note`: it finds an exact piece of text and replaces it. To update `notes/active-work`, replace the section that changed (for example, the whole `## Current state` section) with its new text.';
    updated integer;
BEGIN
    UPDATE templates
       SET content = replace(content, old_text, new_text),
           updated_at = NOW()
     WHERE kind = 'welcome'
       AND name = 'default'
       AND position(old_text IN content) > 0;
    GET DIAGNOSTICS updated = ROW_COUNT;
    IF updated <> 1 THEN
        RAISE EXCEPTION 'MEM-149: expected 1 default welcome template with the save_note sentence, found %', updated;
    END IF;

    UPDATE agent_configuration
       SET startup_instructions = replace(startup_instructions, old_text, new_text)
     WHERE position(old_text IN startup_instructions) > 0;
    GET DIAGNOSTICS updated = ROW_COUNT;
    RAISE NOTICE 'MEM-149: patched startup_instructions for % accounts', updated;
END
$MIGRATION$;

COMMIT;
