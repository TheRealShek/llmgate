CREATE TABLE inference_admissions (
    request_id TEXT PRIMARY KEY CHECK (octet_length(request_id) BETWEEN 1 AND 128),
    key_id BIGINT NOT NULL REFERENCES api_keys(id),
    backend TEXT NOT NULL CHECK (octet_length(backend) BETWEEN 1 AND 128),
    model TEXT CHECK (octet_length(model) BETWEEN 1 AND 256),
    streaming BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    reservation_id TEXT CHECK (octet_length(reservation_id) BETWEEN 1 AND 128),
    reservation_day BIGINT CHECK (reservation_day >= 0),
    reserved_prompt_tokens BIGINT CHECK (reserved_prompt_tokens >= 0),
    reserved_output_tokens BIGINT CHECK (reserved_output_tokens > 0),
    CHECK ((reservation_id IS NULL) = (reservation_day IS NULL)),
    CHECK ((reservation_id IS NULL) = (reserved_prompt_tokens IS NULL)),
    CHECK ((reservation_id IS NULL) = (reserved_output_tokens IS NULL))
);

CREATE INDEX inference_admissions_created_at ON inference_admissions (created_at);
