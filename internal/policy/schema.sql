CREATE TABLE IF NOT EXISTS policies (
    client_id      TEXT             PRIMARY KEY CHECK (char_length(client_id) BETWEEN 1 AND 128),
    capacity       BIGINT           NOT NULL CHECK (capacity >= 1),
    refill_per_sec DOUBLE PRECISION NOT NULL CHECK (refill_per_sec >= 0),
    created_at     TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ      NOT NULL DEFAULT now()
);
