-- MEM-030: Add soft delete for mail (unsend support)
-- Sender can unsend mail before recipient acks it. Sets deleted_at instead of hard deleting.

ALTER TABLE mail ADD COLUMN deleted_at TIMESTAMPTZ;
