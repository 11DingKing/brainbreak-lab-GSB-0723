-- Focus experiment service schema.
--
-- Design notes for the guarantees the service must uphold:
--
--  * Idempotency: focus_events has a UNIQUE constraint on the natural client
--    key (experiment_id, subject_id, device_id, client_seq). InsertEventIfAbsent
--    uses ON CONFLICT DO NOTHING against this, so an event is stored — and thus
--    counted — at most once regardless of retries, reordering or cross-device
--    concurrent uploads.
--
--  * Crypto-shredding: personal data lives ONLY as ciphertext in
--    subjects.sealed_personal, encrypted under a per-subject key stored in
--    subject_keys. Hard delete destroys the key row (leaving a tombstone) and
--    purges derived rows, so personal data cannot be recovered from any derived
--    table even if ciphertext survives in a backup.
--
--  * Cascading delete: derived tables reference subjects with ON DELETE CASCADE
--    so purging a subject removes their events and results atomically.

CREATE TABLE IF NOT EXISTS experiments (
    id          UUID PRIMARY KEY,
    name        TEXT        NOT NULL,
    version     BIGINT      NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-subject data key for crypto-shredding. destroyed=true is a tombstone that
-- makes re-creation impossible and signals "personal data permanently gone".
CREATE TABLE IF NOT EXISTS subject_keys (
    subject_id  UUID PRIMARY KEY,
    key         BYTEA,
    destroyed   BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS subjects (
    experiment_id   UUID        NOT NULL REFERENCES experiments(id) ON DELETE CASCADE,
    id              UUID        NOT NULL,
    auth            TEXT        NOT NULL DEFAULT 'active',
    sealed_personal BYTEA,
    PRIMARY KEY (experiment_id, id)
);

CREATE TABLE IF NOT EXISTS focus_events (
    experiment_id   UUID        NOT NULL,
    subject_id      UUID        NOT NULL,
    device_id       TEXT        NOT NULL,
    client_seq      BIGINT      NOT NULL,
    event_type      TEXT        NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    duration_ms     BIGINT      NOT NULL DEFAULT 0,
    -- Natural idempotency key: one row per client-reported event.
    CONSTRAINT focus_events_idem UNIQUE (experiment_id, subject_id, device_id, client_seq),
    FOREIGN KEY (experiment_id, subject_id)
        REFERENCES subjects(experiment_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS focus_events_subject_idx
    ON focus_events (experiment_id, subject_id, occurred_at);

CREATE TABLE IF NOT EXISTS results (
    experiment_id   UUID        NOT NULL,
    subject_id      UUID        NOT NULL,
    version         BIGINT      NOT NULL,
    digest          TEXT        NOT NULL,
    result_json     JSONB       NOT NULL,
    computed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (experiment_id, subject_id, version),
    FOREIGN KEY (experiment_id, subject_id)
        REFERENCES subjects(experiment_id, id) ON DELETE CASCADE
);
