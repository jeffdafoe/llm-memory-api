-- MEM-014: Discussion readiness / handshake support
-- Adds waiting state, timeout tracking, and required/optional participant roles.

-- timeout_at: when the waiting period expires (computed at creation from config default)
ALTER TABLE discussions ADD COLUMN timeout_at TIMESTAMPTZ;

-- role: whether a participant is required for the discussion to start
ALTER TABLE discussion_participants ADD COLUMN role VARCHAR(20) NOT NULL DEFAULT 'required';
