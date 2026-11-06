-- Fails while any participant row is 'deferred'.
BEGIN;

ALTER TABLE discussion_participants ADD CONSTRAINT chk_discussion_participants_status
    CHECK (((status)::text = ANY ((ARRAY['invited'::character varying, 'joined'::character varying, 'left'::character varying, 'timed_out'::character varying])::text[])));

COMMIT;
