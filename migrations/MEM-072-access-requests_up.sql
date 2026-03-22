-- Access requests from the landing page "Ask for Free Access" form
CREATE TABLE access_requests (
    id SERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL,
    usage_description TEXT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    reviewed_at TIMESTAMP WITH TIME ZONE,
    reviewer_notes TEXT
);

CREATE INDEX idx_access_requests_status ON access_requests(status);
CREATE INDEX idx_access_requests_created ON access_requests(created_at DESC);
