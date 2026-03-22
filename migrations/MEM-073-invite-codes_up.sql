-- Invite codes for gated agent registration
CREATE TABLE invite_codes (
    id SERIAL PRIMARY KEY,
    code VARCHAR(32) NOT NULL UNIQUE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    created_by VARCHAR(255),
    access_request_id INTEGER REFERENCES access_requests(id),
    used_by VARCHAR(255),
    used_at TIMESTAMP WITH TIME ZONE,
    expires_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_invite_codes_code ON invite_codes(code);
CREATE INDEX idx_invite_codes_access_request ON invite_codes(access_request_id);
