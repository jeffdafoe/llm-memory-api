-- MEM-036: Add outcome column to discussions table
-- Server computes outcome from vote history when a discussion ends.
-- Values: consensus (all votes passed or no votes needed), deadlock (no votes passed),
-- partial (mix of passed/failed), abandoned (cancelled/timed_out).

ALTER TABLE discussions
    ADD COLUMN outcome VARCHAR(20);

ALTER TABLE discussions
    ADD CONSTRAINT chk_discussions_outcome
    CHECK (outcome IN ('consensus', 'deadlock', 'partial', 'abandoned'));
