ALTER TABLE usage ADD COLUMN recovered BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE usage ADD CONSTRAINT usage_recovered_outcome CHECK (
    NOT recovered OR (status = 'interrupted' AND prompt_tokens IS NULL
        AND completion_tokens IS NULL AND ttft_ms IS NULL AND http_status = 0)
);

CREATE INDEX usage_created_at_request_id ON usage (created_at, request_id);
