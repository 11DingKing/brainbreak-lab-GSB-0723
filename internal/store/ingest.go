package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"brainbreak-lab/focus/internal/model"
	"brainbreak-lab/focus/internal/rules"
)

// IngestEventInput 是单条事件的入参。
type IngestEventInput struct {
	ClientSeq  int64           `json:"client_seq"`
	DeviceID   string          `json:"device_id"`
	EventType  model.EventType `json:"event_type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// IngestResult 是一次幂等写入的结果。
type IngestResult struct {
	Batch          *model.IngestBatch   `json:"batch"`
	AcceptedCount  int                  `json:"accepted_count"`
	DuplicateCount int                  `json:"duplicate_count"`
	Result         *model.ResultSummary `json:"result"`
}

// ErrDuplicateBatch 表示幂等键已存在，返回的是首次写入结果。
var errDuplicateBatch = errors.New("duplicate batch")

// IngestEvents 在单事务内幂等写入事件并重新计算派生结果。
// 重复的 (experiment_id,device_id,client_seq) 会被 ON CONFLICT 丢弃。
func (db *DB) IngestEvents(ctx context.Context, experimentID uuid.UUID, idempotencyKey string, events []IngestEventInput) (*IngestResult, error) {
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	var result *IngestResult
	err := db.tx(ctx, func(tx pgx.Tx) error {
		exp, sub, loc, err := lockExperimentForWrite(ctx, tx, experimentID)
		if err != nil {
			return err
		}
		// 幂等：若批次已存在，返回当时的结果快照。
		if existing, err := fetchExistingBatchResult(ctx, tx, experimentID, idempotencyKey); err == nil {
			result = existing
			return errDuplicateBatch
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		// 写入新批次。
		now := time.Now().UTC()
		var batchID int64
		err = tx.QueryRow(ctx, `
			INSERT INTO ingest_batches(experiment_id,idempotency_key,received_at,event_count,accepted_count)
			VALUES ($1,$2,$3,$4,0) RETURNING id`,
			experimentID, idempotencyKey, now, len(events)).Scan(&batchID)
		if err != nil {
			return mapError(err)
		}
		if err := db.hook(ctx, "after-batch-insert"); err != nil {
			return err
		}
		// 批量插入事件；重复的丢弃。
		accepted := 0
		// 先收集本批被接受的事件 ID，用于返回。
		for _, ev := range events {
			if !ev.EventType.Valid() {
				return ErrValidation
			}
			if ev.OccurredAt.IsZero() {
				return ErrValidation
			}
			if ev.DeviceID == "" {
				ev.DeviceID = "default"
			}
			payload := ev.Payload
			if len(payload) == 0 {
				payload = json.RawMessage(`{}`)
			}
			eid := uuid.New()
			var inserted bool
			err := tx.QueryRow(ctx, `
				INSERT INTO events(id,batch_id,experiment_id,subject_id,client_seq,device_id,event_type,occurred_at,received_at,payload)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
				ON CONFLICT (experiment_id,device_id,client_seq) DO NOTHING
				RETURNING true`,
				eid, batchID, experimentID, exp.SubjectID, ev.ClientSeq, ev.DeviceID,
				string(ev.EventType), ev.OccurredAt.UTC(), now, payload).Scan(&inserted)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					// 冲突，未插入。
					continue
				}
				return err
			}
			accepted++
		}
		// 更新 accepted_count。
		if _, err := tx.Exec(ctx, `UPDATE ingest_batches SET accepted_count=$1 WHERE id=$2`, accepted, batchID); err != nil {
			return err
		}
		if err := db.hook(ctx, "after-event-insert"); err != nil {
			return err
		}
		// 重新计算派生结果。
		summary, err := recomputeInTx(ctx, tx, exp, sub, loc, batchID)
		if err != nil {
			return err
		}
		if err := db.hook(ctx, "after-recompute"); err != nil {
			return err
		}
		result = &IngestResult{
			Batch: &model.IngestBatch{
				ID:             batchID,
				ExperimentID:   experimentID,
				IdempotencyKey: idempotencyKey,
				ReceivedAt:     now,
				EventCount:     len(events),
				AcceptedCount:  accepted,
			},
			AcceptedCount:  accepted,
			DuplicateCount: len(events) - accepted,
			Result:         summary,
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errDuplicateBatch) {
			return result, nil
		}
		return nil, err
	}
	return result, nil
}

// fetchExistingBatchResult 返回幂等命中时的批次与当时结果。
func fetchExistingBatchResult(ctx context.Context, tx pgx.Tx, experimentID uuid.UUID, key string) (*IngestResult, error) {
	var b model.IngestBatch
	err := tx.QueryRow(ctx, `
		SELECT id,experiment_id,idempotency_key,received_at,event_count,accepted_count
		FROM ingest_batches WHERE experiment_id=$1 AND idempotency_key=$2`, experimentID, key).
		Scan(&b.ID, &b.ExperimentID, &b.IdempotencyKey, &b.ReceivedAt, &b.EventCount, &b.AcceptedCount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	summary, err := fetchResultInTx(ctx, tx, experimentID, b.ID)
	if err != nil {
		return nil, err
	}
	return &IngestResult{
		Batch:          &b,
		AcceptedCount:  b.AcceptedCount,
		DuplicateCount: b.EventCount - b.AcceptedCount,
		Result:         summary,
	}, nil
}

// loadEvents 在 tx 中加载指定实验 batch_id<=upToVersion 的全部事件，按 occurred_at 排序。
func loadEvents(ctx context.Context, tx pgx.Tx, experimentID uuid.UUID, upToVersion int64) ([]model.Event, error) {
	rows, err := tx.Query(ctx, `
		SELECT id,batch_id,experiment_id,subject_id,client_seq,device_id,event_type,occurred_at,received_at,payload
		FROM events
		WHERE experiment_id=$1 AND batch_id<=$2
		ORDER BY occurred_at ASC, client_seq ASC`, experimentID, upToVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		var etype string
		if err := rows.Scan(&e.ID, &e.BatchID, &e.ExperimentID, &e.SubjectID, &e.ClientSeq,
			&e.DeviceID, &etype, &e.OccurredAt, &e.ReceivedAt, &e.Payload); err != nil {
			return nil, err
		}
		e.EventType = model.EventType(etype)
		out = append(out, e)
	}
	return out, rows.Err()
}

// recomputeInTx 从事件重建派生表并写入 results 快照。
func recomputeInTx(ctx context.Context, tx pgx.Tx, exp *model.Experiment, sub *model.Subject, loc *time.Location, version int64) (*model.ResultSummary, error) {
	events, err := loadEvents(ctx, tx, exp.ID, version)
	if err != nil {
		return nil, err
	}
	profile := rules.SubjectProfile{DateOfBirth: sub.DateOfBirth, Timezone: loc, Bedtime: sub.Bedtime}
	days := rules.Evaluate(events, profile)

	// 重建派生表：清空本实验旧记录再写入。
	if _, err := tx.Exec(ctx, `DELETE FROM daily_usage WHERE experiment_id=$1`, exp.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM violations WHERE experiment_id=$1`, exp.ID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	summary := &model.ResultSummary{
		SubjectID:     sub.ID,
		ExperimentID:  exp.ID,
		EventVersion:  version,
		SchemaVersion: rules.SchemaVersion,
		ComputedAt:    now,
		Days:          make([]model.DaySummary, 0, len(days)),
	}
	for _, d := range days {
		if d.TotalSeconds > 0 || d.SessionCount > 0 || len(d.Violations) > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO daily_usage(experiment_id,subject_id,local_date,total_seconds,session_count,event_version,updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				exp.ID, sub.ID, d.Date, d.TotalSeconds, d.SessionCount, version, now); err != nil {
				return nil, err
			}
		}
		vlist := make([]model.ViolationView, 0, len(d.Violations))
		for i := range d.Violations {
			v := d.Violations[i]
			v.ExperimentID = exp.ID
			v.SubjectID = sub.ID
			v.LocalDate = d.Date
			v.EventVersion = version
			if _, err := tx.Exec(ctx, `
				INSERT INTO violations(experiment_id,subject_id,local_date,rule_code,event_id,detail,event_version,created_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				v.ExperimentID, v.SubjectID, v.LocalDate, v.RuleCode, v.EventID, v.Detail, version, now); err != nil {
				return nil, err
			}
			vlist = append(vlist, model.ViolationView{RuleCode: v.RuleCode, Detail: v.Detail})
		}
		summary.Days = append(summary.Days, model.DaySummary{
			Date:         d.Date.Format("2006-01-02"),
			AgeGroup:     d.AgeGroup,
			TotalSeconds: d.TotalSeconds,
			SessionCount: d.SessionCount,
			Violations:   vlist,
		})
		summary.Totals.TotalSeconds += d.TotalSeconds
		summary.Totals.SessionCount += d.SessionCount
		summary.Totals.ViolationCount += len(vlist)
	}
	// 写入结果快照（按版本幂等）。
	summaryBytes, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO results(experiment_id,event_version,schema_version,computed_at,summary)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (experiment_id,event_version) DO UPDATE
			SET schema_version=EXCLUDED.schema_version,
			    computed_at=EXCLUDED.computed_at,
			    summary=EXCLUDED.summary`,
		exp.ID, version, rules.SchemaVersion, now, summaryBytes); err != nil {
		return nil, err
	}
	return summary, nil
}

// Recalc 强制按当前所有事件与给定 schema 版本重算（用于算法升级后的按版本重算）。
func (db *DB) Recalc(ctx context.Context, experimentID uuid.UUID) (*model.ResultSummary, error) {
	var summary *model.ResultSummary
	err := db.tx(ctx, func(tx pgx.Tx) error {
		exp, sub, loc, err := lockExperimentForWrite(ctx, tx, experimentID)
		if err != nil {
			return err
		}
		// 当前最大批次号。
		var version int64
		err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(id),0) FROM ingest_batches WHERE experiment_id=$1`, experimentID).Scan(&version)
		if err != nil {
			return err
		}
		s, err := recomputeInTx(ctx, tx, exp, sub, loc, version)
		if err != nil {
			return err
		}
		summary = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return summary, nil
}

// GetResult 返回指定事件版本的结果快照；version<=0 返回最新。
func (db *DB) GetResult(ctx context.Context, experimentID uuid.UUID, version int64) (*model.ResultSummary, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, mapError(err)
	}
	defer tx.Rollback(ctx)
	if version <= 0 {
		// 取最新 event_version。
		err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(event_version),0) FROM results WHERE experiment_id=$1`, experimentID).Scan(&version)
		if err != nil {
			return nil, mapError(err)
		}
		if version == 0 {
			return nil, ErrNotFound
		}
	}
	return fetchResultInTx(ctx, tx, experimentID, version)
}

func fetchResultInTx(ctx context.Context, tx pgx.Tx, experimentID uuid.UUID, version int64) (*model.ResultSummary, error) {
	var bytes []byte
	var s model.ResultSummary
	err := tx.QueryRow(ctx, `
		SELECT summary FROM results WHERE experiment_id=$1 AND event_version=$2`, experimentID, version).Scan(&bytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(bytes, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
