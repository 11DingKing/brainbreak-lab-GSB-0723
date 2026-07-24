-- 001_init.sql: 专注实验事件处理服务 schema
-- 所有表使用 UUID / BIGSERIAL；事件与派生表均通过 subject_id 关联以便彻底删除。

CREATE TABLE IF NOT EXISTS subjects (
    id              UUID PRIMARY KEY,
    date_of_birth   DATE        NOT NULL,
    timezone        TEXT        NOT NULL,
    bedtime         TIME,
    consent_at      TIMESTAMPTZ NOT NULL,
    withdrawn_at    TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_subjects_deleted ON subjects(deleted_at);

CREATE TABLE IF NOT EXISTS experiments (
    id           UUID PRIMARY KEY,
    subject_id   UUID NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    label        TEXT,
    status       TEXT NOT NULL DEFAULT 'open',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_experiments_subject ON experiments(subject_id);

-- 每个写入批次；id 即单调递增的 event_version
CREATE TABLE IF NOT EXISTS ingest_batches (
    id              BIGSERIAL PRIMARY KEY,
    experiment_id   UUID NOT NULL REFERENCES experiments(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_count     INTEGER NOT NULL DEFAULT 0,
    accepted_count  INTEGER NOT NULL DEFAULT 0,
    UNIQUE (experiment_id, idempotency_key)
);

-- 事件表：不可变。同一实验下 (client_seq, device_id) 唯一，保证幂等。
CREATE TABLE IF NOT EXISTS events (
    id              UUID PRIMARY KEY,
    batch_id        BIGINT NOT NULL REFERENCES ingest_batches(id) ON DELETE CASCADE,
    experiment_id   UUID NOT NULL REFERENCES experiments(id) ON DELETE CASCADE,
    subject_id      UUID NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    client_seq      BIGINT NOT NULL,
    device_id       TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT uq_events_client_seq UNIQUE (experiment_id, device_id, client_seq)
);

CREATE INDEX IF NOT EXISTS idx_events_experiment_occurred ON events(experiment_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_events_batch ON events(batch_id);

-- 派生：按日累计（按 subject 时区折算到本地日期）
CREATE TABLE IF NOT EXISTS daily_usage (
    experiment_id   UUID NOT NULL REFERENCES experiments(id) ON DELETE CASCADE,
    subject_id      UUID NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    local_date      DATE NOT NULL,
    total_seconds   BIGINT NOT NULL DEFAULT 0,
    session_count   INTEGER NOT NULL DEFAULT 0,
    event_version   BIGINT NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (experiment_id, local_date)
);

-- 派生：违规记录
CREATE TABLE IF NOT EXISTS violations (
    id              BIGSERIAL PRIMARY KEY,
    experiment_id   UUID NOT NULL REFERENCES experiments(id) ON DELETE CASCADE,
    subject_id      UUID NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    local_date      DATE NOT NULL,
    rule_code       TEXT NOT NULL,
    event_id        UUID,
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,
    event_version   BIGINT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_violations_experiment ON violations(experiment_id);

-- 派生：版本化结果（可重放）
CREATE TABLE IF NOT EXISTS results (
    experiment_id   UUID NOT NULL REFERENCES experiments(id) ON DELETE CASCADE,
    event_version   BIGINT NOT NULL,
    schema_version  INTEGER NOT NULL,
    computed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    summary         JSONB NOT NULL,
    PRIMARY KEY (experiment_id, event_version)
);

-- 仅保留不含身份信息的删除审计（彻底删除后无法回查个人数据）
CREATE TABLE IF NOT EXISTS deletion_audit (
    token        UUID PRIMARY KEY,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    note         TEXT
);
