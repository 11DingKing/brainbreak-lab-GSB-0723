package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"brainbreak-lab/focus/internal/httpapi"
	"brainbreak-lab/focus/internal/model"
	"brainbreak-lab/focus/internal/rules"
	"brainbreak-lab/focus/internal/service"
	"brainbreak-lab/focus/internal/store"
)

func testDSN() string {
	return os.Getenv("DATABASE_URL")
}

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Fatal("DATABASE_URL not set; TestMain should have provisioned PostgreSQL")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	schema := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	cfg.MaxConns = 8
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx,
			fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q; SET search_path TO %q`, schema, schema))
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("cannot open db: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("cannot ping db: %v", err)
	}
	db := store.New(pool)
	if err := db.Migrate(ctx); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema))
		pool.Close()
	})
	return db
}

// 便捷构造
func createSubjectReturnID(t *testing.T, svc *service.Service, tz, dob, bedtime string) string {
	t.Helper()
	sub, err := svc.CreateSubject(context.Background(), service.CreateSubjectRequest{
		DateOfBirth: dob, Timezone: tz, Bedtime: bedtime,
	})
	if err != nil {
		t.Fatalf("create subject: %v", err)
	}
	return sub.ID.String()
}

func createExperiment(t *testing.T, svc *service.Service, subjectID string) string {
	t.Helper()
	e, err := svc.CreateExperiment(context.Background(), service.CreateExperimentRequest{SubjectID: subjectID})
	if err != nil {
		t.Fatalf("create experiment: %v", err)
	}
	return e.ID.String()
}

func ingest(t *testing.T, svc *service.Service, expID string, key string, evs []store.IngestEventInput) *store.IngestResult {
	t.Helper()
	r, err := svc.IngestEvents(context.Background(), expID, service.IngestEventsRequest{
		IdempotencyKey: key, Events: evs,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return r
}

func sess(seq int64, device string, at time.Time, dur int64) store.IngestEventInput {
	p, _ := json.Marshal(map[string]int64{"duration_seconds": dur})
	return store.IngestEventInput{
		ClientSeq: seq, DeviceID: device, EventType: model.EventViewingSession,
		OccurredAt: at, Payload: p,
	}
}

func inst(seq int64, device string, et model.EventType, at time.Time) store.IngestEventInput {
	return store.IngestEventInput{
		ClientSeq: seq, DeviceID: device, EventType: et, OccurredAt: at, Payload: json.RawMessage(`{}`),
	}
}

func TestEndToEndAdultDaily(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	ingest(t, svc, eid, "k1", []store.IngestEventInput{
		sess(1, "phone", day, 50*60),
		sess(2, "phone", day.Add(2*time.Hour), 20*60),
	})
	r, err := svc.GetResult(context.Background(), eid, 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if r.EventVersion != 1 {
		t.Fatalf("version=%d", r.EventVersion)
	}
	if r.Totals.TotalSeconds != 70*60 {
		t.Fatalf("total=%d", r.Totals.TotalSeconds)
	}
	found := map[string]bool{}
	for _, d := range r.Days {
		for _, v := range d.Violations {
			found[v.RuleCode] = true
		}
	}
	if !found[rules.RuleAdultDailyExceeded] || !found[rules.RuleAdultSessionTooLong] {
		t.Fatalf("missing violations: %+v", found)
	}
}

func TestIdempotencyAndDuplicateEvents(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	evs := []store.IngestEventInput{sess(1, "phone", day, 10*60)}
	first := ingest(t, svc, eid, "idem-1", evs)
	if first.AcceptedCount != 1 {
		t.Fatalf("first accepted=%d", first.AcceptedCount)
	}
	// 重试同幂等键应返回同样结果，accepted=1。
	retry := ingest(t, svc, eid, "idem-1", evs)
	if retry.AcceptedCount != 1 || retry.Batch.ID != first.Batch.ID {
		t.Fatalf("retry not idempotent: %+v vs %+v", retry.Batch, first.Batch)
	}
	// 不同幂等键但相同 client_seq/device：应被去重。
	second := ingest(t, svc, eid, "idem-2", evs)
	if second.AcceptedCount != 0 || second.DuplicateCount != 1 {
		t.Fatalf("expected duplicate skip, got accepted=%d dup=%d", second.AcceptedCount, second.DuplicateCount)
	}
	// 总累计应仍为 10 分钟。
	r, _ := svc.GetResult(context.Background(), eid, 0)
	if r.Totals.TotalSeconds != 10*60 {
		t.Fatalf("total after duplicates=%d", r.Totals.TotalSeconds)
	}
}

func TestLateEventCorrectsResult(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	// 先写一个合规 session
	ingest(t, svc, eid, "k1", []store.IngestEventInput{sess(10, "phone", day, 10*60)})
	r1, _ := svc.GetResult(context.Background(), eid, 0)
	if r1.Totals.TotalSeconds != 10*60 || r1.Totals.ViolationCount != 0 {
		t.Fatalf("unexpected %+v", r1.Totals)
	}
	// 迟到事件：client_seq=5，发生时间早于已写入事件；新累计 = 10+55=65 分钟。
	ingest(t, svc, eid, "k2", []store.IngestEventInput{
		sess(5, "phone", day.Add(-30*time.Minute), 55*60),
		inst(7, "phone", model.EventCardView, day.Add(-25*time.Minute)),
	})
	r2, _ := svc.GetResult(context.Background(), eid, 0)
	if r2.Totals.TotalSeconds != 65*60 {
		t.Fatalf("late event not incorporated: total=%d", r2.Totals.TotalSeconds)
	}
	if r2.EventVersion != 2 {
		t.Fatalf("version=%d", r2.EventVersion)
	}
	// 旧版本快照仍可重放。
	old, _ := svc.GetResult(context.Background(), eid, 1)
	if old.Totals.TotalSeconds != 10*60 {
		t.Fatalf("replay v1 total=%d", old.Totals.TotalSeconds)
	}
}

func TestReplayAcrossVersions(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	for i := int64(0); i < 5; i++ {
		ingest(t, svc, eid, fmt.Sprintf("k%d", i), []store.IngestEventInput{
			sess(i+1, "phone", day.Add(time.Duration(i)*time.Hour), 10*60),
		})
	}
	for v := int64(1); v <= 5; v++ {
		r, err := svc.GetResult(context.Background(), eid, v)
		if err != nil {
			t.Fatalf("version %d: %v", v, err)
		}
		if r.Totals.TotalSeconds != v*10*60 {
			t.Fatalf("version %d total=%d want %d", v, r.Totals.TotalSeconds, v*10*60)
		}
	}
}

func TestRecalc(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	ingest(t, svc, eid, "k1", []store.IngestEventInput{sess(1, "phone", day, 10*60)})
	// 直接在 DB 层污染 daily_usage，验证 recalc 修复。
	_, err := db.Pool().Exec(context.Background(),
		`UPDATE daily_usage SET total_seconds=99999 WHERE experiment_id=$1`, mustUUID(eid))
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	r, err := svc.Recalc(context.Background(), eid)
	if err != nil {
		t.Fatalf("recalc: %v", err)
	}
	if r.Totals.TotalSeconds != 10*60 {
		t.Fatalf("recalc total=%d", r.Totals.TotalSeconds)
	}
}

func TestWithdrawAndDelete(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	ingest(t, svc, eid, "k1", []store.IngestEventInput{sess(1, "phone", day, 10*60)})

	if err := svc.WithdrawConsent(context.Background(), sid); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	// 撤回后写入应被拒绝。
	_, err := svc.IngestEvents(context.Background(), eid, service.IngestEventsRequest{
		IdempotencyKey: "k2", Events: []store.IngestEventInput{sess(2, "phone", day.Add(time.Hour), 5*60)},
	})
	if err == nil || !strings.Contains(err.Error(), "consent") {
		t.Fatalf("expected consent error, got %v", err)
	}

	// 彻底删除
	token, err := svc.DeleteSubject(context.Background(), sid)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if token == uuid.Nil {
		t.Fatalf("zero token")
	}
	// 所有派生表不得残留 subject 相关数据。
	pool := db.Pool()
	tables := []string{"subjects", "experiments", "ingest_batches", "events", "daily_usage", "violations", "results"}
	for _, tbl := range tables {
		var cnt int
		q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)
		if err := pool.QueryRow(context.Background(), q).Scan(&cnt); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if cnt != 0 {
			t.Fatalf("residual rows in %s after delete: %d", tbl, cnt)
		}
	}
	// 审计表只记录无身份信息的 token。
	var cnt int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM deletion_audit WHERE token=$1`, token).Scan(&cnt); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("audit row missing")
	}
}

func TestTimezoneCrossDay(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	// 上海时区：22:00 UTC = 次日 06:00 上海，应计入次日。
	sid := createSubjectReturnID(t, svc, "Asia/Shanghai", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	atUTC := time.Date(2026, 7, 24, 22, 0, 0, 0, time.UTC) // 2026-07-25 06:00 SH
	ingest(t, svc, eid, "k1", []store.IngestEventInput{sess(1, "phone", atUTC, 10*60)})
	r, _ := svc.GetResult(context.Background(), eid, 0)
	if len(r.Days) != 1 || r.Days[0].Date != "2026-07-25" {
		t.Fatalf("expected 2026-07-25, got %+v", r.Days)
	}
}

func TestConcurrentCrossDeviceIngest(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)

	const workers = 8
	const perWorker = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wi int) {
			defer wg.Done()
			dev := fmt.Sprintf("dev-%d", wi)
			for i := 0; i < perWorker; i++ {
				seq := int64(wi*perWorker + i)
				key := fmt.Sprintf("k-%d-%d", wi, i)
				evs := []store.IngestEventInput{sess(seq, dev, day.Add(time.Duration(seq)*time.Minute), 60)}
				_, err := svc.IngestEvents(context.Background(), eid, service.IngestEventsRequest{
					IdempotencyKey: key, Events: evs,
				})
				if err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent ingest err: %v", e)
	}
	r, _ := svc.GetResult(context.Background(), eid, 0)
	if r.Totals.SessionCount != workers*perWorker {
		t.Fatalf("sessions=%d want %d", r.Totals.SessionCount, workers*perWorker)
	}
	if r.Totals.TotalSeconds != int64(workers*perWorker)*60 {
		t.Fatalf("total=%d", r.Totals.TotalSeconds)
	}
}

// 并发重复提交同一幂等键：只能产生一个批次，结果一致。
func TestConcurrentSameIdempotencyKey(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)

	const workers = 12
	var wg sync.WaitGroup
	results := make([]*store.IngestResult, workers)
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := svc.IngestEvents(context.Background(), eid, service.IngestEventsRequest{
				IdempotencyKey: "same-key",
				Events:         []store.IngestEventInput{sess(1, "phone", day, 10*60)},
			})
			if err != nil {
				errs <- err
				return
			}
			results[i] = r
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("err: %v", e)
	}
	var first *store.IngestResult
	for _, r := range results {
		if r == nil {
			t.Fatalf("nil result")
		}
		if first == nil {
			first = r
			continue
		}
		if r.Batch.ID != first.Batch.ID {
			t.Fatalf("batch id mismatch: %d vs %d", r.Batch.ID, first.Batch.ID)
		}
	}
	res, _ := svc.GetResult(context.Background(), eid, 0)
	if res.Totals.TotalSeconds != 10*60 {
		t.Fatalf("total after concurrent same-key=%d", res.Totals.TotalSeconds)
	}
}

// 故障注入：在事件插入后、结果写入前让钩子失败，验证事务回滚（无残留事件/结果）。
func TestFaultInjectionRollback(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")
	eid := createExperiment(t, svc, sid)
	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)

	db.SetTestHook(func(ctx context.Context, phase string) error {
		if phase == "after-event-insert" {
			return errors.New("injected failure")
		}
		return nil
	})
	_, err := svc.IngestEvents(context.Background(), eid, service.IngestEventsRequest{
		IdempotencyKey: "k-fail", Events: []store.IngestEventInput{sess(1, "phone", day, 10*60)},
	})
	if err == nil {
		t.Fatalf("expected error from injected failure")
	}
	db.SetTestHook(nil)
	// 验证没有任何事件/批次/结果残留。
	pool := db.Pool()
	var ev, ba, rs int
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM events WHERE experiment_id=$1`, mustUUID(eid)).Scan(&ev)
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM ingest_batches WHERE experiment_id=$1`, mustUUID(eid)).Scan(&ba)
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM results WHERE experiment_id=$1`, mustUUID(eid)).Scan(&rs)
	if ev != 0 || ba != 0 || rs != 0 {
		t.Fatalf("rollback incomplete: events=%d batches=%d results=%d", ev, ba, rs)
	}
	// 随后可正常写入。
	ingest(t, svc, eid, "k-ok", []store.IngestEventInput{sess(1, "phone", day, 10*60)})
}

// 属性测试：对同一批事件的任意写入顺序与分批方式，最终累计与违规集合一致。
func TestPropertyCommutativity(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	sid := createSubjectReturnID(t, svc, "UTC", "1990-05-01", "")

	day := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	events := []store.IngestEventInput{
		sess(1, "phone", day, 10*60),
		sess(2, "phone", day.Add(1*time.Hour), 16*60),
		sess(3, "phone", day.Add(2*time.Hour), 35*60),
		sess(4, "phone", day.Add(3*time.Hour), 5*60),
		inst(5, "phone", model.EventAttentionSwitch, day.Add(5*time.Minute)),
		inst(6, "phone", model.EventSlowReading, day.Add(10*time.Minute)),
		inst(7, "tablet", model.EventCardView, day.Add(15*time.Minute)),
	}

	run := func(order []int) *model.ResultSummary {
		eid := createExperiment(t, svc, sid)
		// 单批写入（乱序）
		shuffled := make([]store.IngestEventInput, len(order))
		for i, idx := range order {
			shuffled[i] = events[idx]
		}
		ingest(t, svc, eid, "single", shuffled)
		r, err := svc.GetResult(context.Background(), eid, 0)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return r
	}
	runSplit := func(splits ...int) *model.ResultSummary {
		eid := createExperiment(t, svc, sid)
		prev := 0
		for i, s := range splits {
			ingest(t, svc, eid, fmt.Sprintf("b%d", i), events[prev:s])
			prev = s
		}
		r, err := svc.GetResult(context.Background(), eid, 0)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return r
	}

	canonical := run([]int{0, 1, 2, 3, 4, 5, 6})
	permOrders := [][]int{
		{6, 5, 4, 3, 2, 1, 0},
		{3, 0, 5, 1, 6, 2, 4},
		{0, 6, 1, 5, 2, 4, 3},
	}
	for _, p := range permOrders {
		got := run(p)
		assertSameTotals(t, canonical, got)
	}
	for _, splits := range [][]int{{2, 5, 7}, {1, 3, 6, 7}, {7}} {
		got := runSplit(splits...)
		assertSameTotals(t, canonical, got)
	}
}

func assertSameTotals(t *testing.T, a, b *model.ResultSummary) {
	t.Helper()
	if a.Totals.TotalSeconds != b.Totals.TotalSeconds ||
		a.Totals.SessionCount != b.Totals.SessionCount ||
		a.Totals.ViolationCount != b.Totals.ViolationCount {
		t.Fatalf("totals differ: %+v vs %+v", a.Totals, b.Totals)
	}
	if len(a.Days) != len(b.Days) {
		t.Fatalf("days differ: %d vs %d", len(a.Days), len(b.Days))
	}
	for i := range a.Days {
		if a.Days[i].Date != b.Days[i].Date ||
			a.Days[i].TotalSeconds != b.Days[i].TotalSeconds ||
			len(a.Days[i].Violations) != len(b.Days[i].Violations) {
			t.Fatalf("day %d differ: %+v vs %+v", i, a.Days[i], b.Days[i])
		}
	}
}

// 非诊断性输出：错误响应不得包含 SQL 错误、文件路径或堆栈信息。
func TestNonDiagnosticErrors(t *testing.T) {
	db := openTestDB(t)
	svc := service.New(db)
	h := httpapi.New(svc)
	r := httpapi.NewRouter(h)
	srv := httptest.NewServer(r)
	defer srv.Close()

	// 无效 JSON -> 400
	resp, err := http.Post(srv.URL+"/v1/experiments", "application/json", bytes.NewBufferString(`{bad json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	s := toJSON(body)
	for _, leak := range []string{"pgx", "SQL", "runtime/", ".go:", "goroutine", "ERROR:"} {
		if strings.Contains(s, leak) {
			t.Fatalf("response leaks %q: %s", leak, s)
		}
	}

	// 不存在资源 -> 404，不含 SQL
	resp2, err := http.Get(srv.URL + "/v1/experiments/00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("status=%d", resp2.StatusCode)
	}
	var b2 map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&b2)
	if strings.Contains(toJSON(b2), "sql") || strings.Contains(toJSON(b2), "pg_") {
		t.Fatalf("leak: %s", toJSON(b2))
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mustUUID(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
