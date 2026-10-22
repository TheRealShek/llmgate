CREATE TABLE usage (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE CHECK (octet_length(request_id) BETWEEN 1 AND 128),
    key_id BIGINT NOT NULL REFERENCES api_keys(id),
    backend TEXT NOT NULL CHECK (octet_length(backend) BETWEEN 1 AND 128),
    model TEXT CHECK (octet_length(model) BETWEEN 1 AND 256),
    streaming BOOLEAN NOT NULL,
    prompt_tokens BIGINT CHECK (prompt_tokens >= 0),
    completion_tokens BIGINT CHECK (completion_tokens >= 0),
    observed_sse_chunks BIGINT NOT NULL CHECK (observed_sse_chunks >= 0),
    ttft_ms DOUBLE PRECISION CHECK (ttft_ms >= 0 AND ttft_ms <= total_ms),
    total_ms DOUBLE PRECISION NOT NULL CHECK (total_ms >= 0 AND total_ms < 'Infinity'::float8),
    http_status INTEGER NOT NULL CHECK (http_status = 0 OR http_status BETWEEN 200 AND 599),
    status TEXT NOT NULL CHECK (status IN ('completed', 'failed', 'canceled', 'interrupted')),
    created_at TIMESTAMPTZ NOT NULL,
    CHECK ((prompt_tokens IS NULL) = (completion_tokens IS NULL)),
    CHECK (streaming OR (observed_sse_chunks = 0 AND ttft_ms IS NULL))
);

CREATE INDEX usage_key_created_at ON usage (key_id, created_at);
