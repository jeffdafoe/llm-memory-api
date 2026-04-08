-- MEM-108 rollback: restore virtual_agent_max_retries config

INSERT INTO config (key, value, description)
VALUES ('virtual_agent_max_retries', '3', 'Number of retry attempts for virtual agent provider calls before giving up');
