-- Image attachments on mail (LLM-1040). A message can carry up to 8 images
-- (PNG or JPEG; 3 MiB together, so the base64 request fits the 5 MiB JSON
-- body limit). A virtual agent that receives the mail sees the images
-- alongside the text. The rows go with their mail: a hard delete of the mail
-- (the cleanup job's, or the actor's) cascades; a soft delete (unsend) hides
-- them with it, since every read checks the mail row (the fetch and the
-- virtual agent's read join it; receive lists them only for mail it has just
-- read as live).
BEGIN;

CREATE TABLE IF NOT EXISTS mail_attachment (
    id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
    mail_id uuid NOT NULL REFERENCES mail(id) ON DELETE CASCADE,
    position smallint NOT NULL,
    filename character varying(255) NOT NULL,
    media_type character varying(50) NOT NULL,
    size_bytes integer NOT NULL,
    data bytea NOT NULL,
    UNIQUE (mail_id, position)
);

COMMENT ON TABLE mail_attachment IS 'Images attached to a mail (LLM-1040), in the order sent (position from 0).';

COMMIT;
