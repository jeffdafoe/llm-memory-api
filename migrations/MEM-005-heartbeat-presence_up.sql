-- MEM-005: Heartbeat/presence — adds last_seen to agents for online/offline tracking

ALTER TABLE agents ADD COLUMN last_seen TIMESTAMPTZ;
