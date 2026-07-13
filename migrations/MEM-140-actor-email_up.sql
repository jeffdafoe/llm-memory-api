-- MEM-140: carry the account's email forward onto actors (LLM-219).
--
-- Every real account is born from an access_request that holds the person's
-- email (landing-page "Ask for Free Access" — auto-approved when
-- open_registration is on, else admin-approved). But registration only ever
-- wrote back invite_codes.used_by = '<handle string>', so the account itself
-- carried NO identity: actors has never had an email column. Recovering "who is
-- this account / what's their email" (the sirius42 stand-down) meant hand-
-- joining access_requests -> invite_codes.used_by -> actors.name, a fragile
-- name-string chain that breaks the moment two requests are approved close
-- together.
--
-- Fix: a nullable email on actors, copied forward at account-creation time
-- (registration + admin /admin/actors/create), so the account self-describes.
-- Nullable because sim NPCs (zbbs realm), utility bots, and legacy accounts
-- have no human behind them. Not UNIQUE: one person may own several agents,
-- and sub-agents inherit their creator's email.
--
-- Backfill walks the existing (reliable) chain: for any invite code that was
-- used (used_by = an actor name) and is tied to an access_request, stamp that
-- request's email onto the matching actor. Accounts that registered before the
-- invite flow, or whose used_by was never populated (the sirius42/greg cases),
-- fall through this join and are reconciled by hand against live data.

BEGIN;

ALTER TABLE actors ADD COLUMN email VARCHAR(255);

-- Recovery lookup: email -> account. Partial (the column is NULL for every
-- sim/bot/legacy row, which is most of them).
CREATE INDEX idx_actors_email ON actors (email) WHERE email IS NOT NULL;

-- Backfill from the existing access_request -> invite_code -> used_by chain.
-- Only stamp accounts whose chain resolves to a SINGLE distinct email: an actor
-- name that matches invite codes carrying two different emails is exactly the
-- ambiguous historical case this ticket calls out, and a bare UPDATE ... FROM
-- join would pick one arbitrarily. Ambiguous names (email_count > 1) and names
-- with no chain are left NULL for manual reconcile against live data.
WITH resolved AS (
    SELECT a.id AS actor_id,
           MIN(ar.email)              AS email,
           COUNT(DISTINCT ar.email)   AS email_count
    FROM actors a
    JOIN invite_codes ic     ON ic.used_by = a.name
    JOIN access_requests ar  ON ar.id = ic.access_request_id
    WHERE a.email IS NULL
      AND ar.email IS NOT NULL
    GROUP BY a.id
)
UPDATE actors a
SET email = r.email
FROM resolved r
WHERE r.actor_id = a.id
  AND r.email_count = 1;

COMMIT;
