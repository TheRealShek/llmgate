CREATE TABLE api_keys (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    key_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
    name TEXT NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 128),
    requests_per_min BIGINT NOT NULL CHECK (requests_per_min > 0),
    tokens_per_day BIGINT NOT NULL CHECK (tokens_per_day > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
