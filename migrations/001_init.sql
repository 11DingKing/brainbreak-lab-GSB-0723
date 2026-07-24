-- Migration 001: Initial schema for BrainBreak Lab - Focus Experiment Event Processing Service

-- Users table: stores user profile with birthdate and timezone for age calculation
CREATE TABLE IF NOT EXISTS users (
    id           UUID PRIMARY KEY,
    birth_date   DATE NOT NULL,
    timezone     VARCHAR(64) NOT NULL DEFAULT 'UTC',
    bedtime      VARCHAR(5) NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Experiments table: experiment metadata with versioning
CREATE TABLE IF NOT EXISTS experiments (
    id           UUID PRIMARY KEY,
    version      INT NOT NULL DEFAULT 1,
    name         VARCHAR(256) NOT NULL,
    config       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Raw events: append-only immutable log of all events (used for replay and recalculation)
CREATE TABLE IF NOT EXISTS raw_events (
    id             BIGSERIAL PRIMARY KEY,
    event_id       UUID NOT NULL,
    user_id        UUID NOT NULL REFERENCES users(id),
    experiment_id  UUID NOT NULL REFERENCES experiments(id),
    device_id      VARCHAR(128) NOT NULL,
    client_seq     BIGINT NOT NULL,
    event_type     VARCHAR(32) NOT NULL,
    occurred_at    TIMESTAMPTZ NOT NULL,
    received_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    payload        JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Idempotency: same logical event from same device must be unique
    CONSTRAINT uq_raw_events_dedup UNIQUE (user_id, experiment_id, device_id, client_seq, event_type),
    CONSTRAINT uq_raw_events_event_id UNIQUE (event_id)
);

CREATE INDEX IF NOT EXISTS idx_raw_events_user_exp ON raw_events(user_id, experiment_id);
CREATE INDEX IF NOT EXISTS idx_raw_events_occurred ON raw_events(occurred_at);

-- Event ingestion log: tracks whether each event was accepted as new or rejected as duplicate
CREATE TABLE IF NOT EXISTS event_ingestion_log (
    event_id    UUID PRIMARY KEY REFERENCES raw_events(event_id),
    user_id     UUID NOT NULL,
    experiment_id UUID NOT NULL,
    accepted    BOOLEAN NOT NULL,
    version     INT NOT NULL,
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_eil_user_exp ON event_ingestion_log(user_id, experiment_id);

-- Authorization grants: tracks consent/authorization for experiments
CREATE TABLE IF NOT EXISTS authorization_grants (
    user_id        UUID NOT NULL REFERENCES users(id),
    experiment_id  UUID NOT NULL REFERENCES experiments(id),
    granted_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at     TIMESTAMPTZ,
    PRIMARY KEY (user_id, experiment_id)
);

-- Daily aggregates: per-user per-experiment per-day (in user timezone) aggregated stats
-- Versioned to support recalculation when late events arrive
CREATE TABLE IF NOT EXISTS daily_aggregates (
    id                       BIGSERIAL PRIMARY KEY,
    user_id                  UUID NOT NULL,
    experiment_id            UUID NOT NULL,
    user_date                DATE NOT NULL,
    total_duration_seconds   BIGINT NOT NULL DEFAULT 0,
    session_count            INT NOT NULL DEFAULT 0,
    longest_session_seconds  BIGINT NOT NULL DEFAULT 0,
    card_view_count          INT NOT NULL DEFAULT 0,
    attention_switch_count   INT NOT NULL DEFAULT 0,
    slow_reading_count       INT NOT NULL DEFAULT 0,
    watching_session_count   INT NOT NULL DEFAULT 0,
    violations               JSONB NOT NULL DEFAULT '[]'::jsonb,
    version                  INT NOT NULL,
    computed_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_daily_agg UNIQUE (user_id, experiment_id, user_date, version)
);

CREATE INDEX IF NOT EXISTS idx_daily_agg_user_exp ON daily_aggregates(user_id, experiment_id, version);

-- Experiment results: final computed results per version
CREATE TABLE IF NOT EXISTS experiment_results (
    id                        BIGSERIAL PRIMARY KEY,
    user_id                   UUID NOT NULL,
    experiment_id             UUID NOT NULL,
    version                   INT NOT NULL,
    result_json               JSONB NOT NULL DEFAULT '{}'::jsonb,
    total_duration_seconds    BIGINT NOT NULL DEFAULT 0,
    total_card_views          INT NOT NULL DEFAULT 0,
    total_attention_switches  INT NOT NULL DEFAULT 0,
    total_slow_reading        INT NOT NULL DEFAULT 0,
    total_watching_sessions   INT NOT NULL DEFAULT 0,
    violation_count           INT NOT NULL DEFAULT 0,
    computed_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_exp_results UNIQUE (user_id, experiment_id, version)
);

CREATE INDEX IF NOT EXISTS idx_results_user_exp ON experiment_results(user_id, experiment_id, version);

-- Deletion records: audit trail for hard deletes (GDPR). After deletion,
-- no personal data remains in any other table; only this anonymous audit entry.
CREATE TABLE IF NOT EXISTS deletion_records (
    id          UUID PRIMARY KEY,
    deleted_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    scope       VARCHAR(32) NOT NULL, -- 'user' or 'experiment'
    scope_hash  VARCHAR(64) NOT NULL  -- SHA-256 of original user_id/experiment_id for audit (non-reversible mapping)
);
