# BrainBreak Lab — 专注实验事件处理服务

接收并处理卡片浏览、注意切换、慢阅读答题与观看会话四类客户端事件，在事件重复、乱序、延迟到达及跨设备并发上传时仍能幂等生成可重放的实验结果。

## 技术栈

- **语言**：Go 1.23（标准库 `net/http` + Gin）
- **数据库**：PostgreSQL 16
- **无消息队列、无外部 SaaS**
- **迁移**：启动时自动执行（文件系统优先，回退到 embed.FS）

## 规则引擎

| 年龄组 | 每日上限 | 单次上限 | 额外规则 |
|--------|----------|----------|----------|
| 成人 (≥18) | 60 分钟 | 15 分钟 | — |
| 青少年 (13–17) | 30 分钟 | — | 睡前 1 小时禁刷 |
| 儿童 (<13) | — | 10 分钟 | — |

年龄按用户时区动态计算；跨日按用户时区本地日期分桶。

## 快速启动

### Docker Compose（推荐）

```bash
docker compose up -d --build
```

服务监听 `http://localhost:8080`，PostgreSQL 监听 `localhost:5432`。

### 本地开发

```bash
# 1. 启动 PostgreSQL（需要本地有 Docker）
docker run --name brainbreak-pg \
  -e POSTGRES_USER=brainbreak \
  -e POSTGRES_PASSWORD=brainbreak \
  -e POSTGRES_DB=brainbreak \
  -p 5432:5432 -d postgres:16-alpine

# 2. 构建并运行
export PATH="/opt/homebrew/opt/go@1.23/bin:$PATH"   # macOS Homebrew
GOTOOLCHAIN=local go build -o brainbreak-server ./cmd/server/
./brainbreak-server
```

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `DATABASE_URL` | `postgres://brainbreak:brainbreak@localhost:5432/brainbreak?sslmode=disable` | PostgreSQL 连接串 |
| `SERVER_ADDR` | `:8080` | HTTP 监听地址 |
| `MIGRATIONS_DIR` | 自动探测 | SQL 迁移目录；留空则使用嵌入迁移 |
| `SHUTDOWN_TIMEOUT` | `10s` | 优雅关闭超时 |

### 迁移目录探测顺序

1. `$MIGRATIONS_DIR` 环境变量
2. 可执行文件同级 `migrations/` 目录
3. 当前工作目录 `migrations/`
4. 嵌入到二进制的迁移（**兜底**，无需外部文件即可运行）

迁移失败将通过 `log.Fatalf` 终止启动，不会以半初始化状态运行。

## API 接口

### 用户管理

```http
POST /api/v1/users
Content-Type: application/json

{
  "birth_date": "2010-06-15",
  "timezone":   "Asia/Shanghai",
  "bedtime":    "22:00"
}
```

```http
DELETE /api/v1/users/{userId}
```

彻底删除用户：级联清除 `raw_events`、`event_ingestion_log`、`daily_aggregates`、`experiment_results`、`authorization_grants`、`users` 所有记录，仅向 `deletion_records` 写入一条 SHA-256 匿名审计条目。**删除后无法从任何派生表恢复个人数据。**

### 实验管理

```http
POST /api/v1/experiments
Content-Type: application/json

{"name": "focus-experiment-v1", "config": {}}
```

```http
DELETE /api/v1/experiments/{expId}
```

### 授权

```http
POST /api/v1/users/{userId}/experiments/{expId}/authorize
POST /api/v1/users/{userId}/experiments/{expId}/revoke
```

未授权或已撤回的用户写入事件返回 `403 Forbidden`。

### 批量事件写入

```http
POST /api/v1/users/{userId}/experiments/{expId}/events
Content-Type: application/json

{
  "events": [
    {
      "event_id":      "uuid-v4",
      "user_id":       "{userId}",
      "experiment_id": "{expId}",
      "device_id":     "phone-abc",
      "client_seq":    1,
      "event_type":    "watching_session",
      "occurred_at":   "2026-07-20T10:00:00Z",
      "payload":       {"duration_seconds": 600}
    }
  ]
}
```

**事件类型**：`card_view`、`attention_switch`、`slow_reading_answer`、`watching_session`

**幂等保证**：同一 `(user_id, experiment_id, device_id, client_seq, event_type)` 及同一 `event_id` 均唯一约束；重复事件计入 `duplicate` 字段，不产生副作用。

**乱序/延迟处理**：写入后立即全量重算；迟到事件触发新版本号，历史版本可查询。

响应：
```json
{"accepted": 1, "duplicate": 0, "version": 2}
```

### 结果查询

```http
GET /api/v1/users/{userId}/experiments/{expId}/result?version=2&daily=true
```

- `version` 省略则返回最新版本
- `daily=true` 附带按用户时区分日的聚合数据

### 版本重算

```http
POST /api/v1/users/{userId}/experiments/{expId}/recalculate/{version}
```

基于该版本及之前的事件重新计算（用于规则修正后回溯）。

### 全量重放

```http
POST /api/v1/users/{userId}/experiments/{expId}/replay
```

从 `raw_events` 全量重放，生成最新版本结果。

### 健康检查

```http
GET /health → {"status":"ok"}
```

## 设计要点

### 幂等性

- `raw_events` 表对 `event_id` 及 `(user_id, experiment_id, device_id, client_seq, event_type)` 双重唯一约束
- `ON CONFLICT DO NOTHING` 保证并发下精确一次写入
- PostgreSQL advisory lock（`pg_advisory_xact_lock`）串行化同一 (user, experiment) 的结果计算

### 版本化与可重放

- 每次事件批写入生成一个递增版本号
- `daily_aggregates` 和 `experiment_results` 均带 `version` 字段，旧版本永久保留
- `/replay` 从不可变的 `raw_events` 全量重建，确保结果可审计复现

### 数据删除

- 硬删除在 `SERIALIZABLE` 事务中执行
- 删除后仅保留 `deletion_records` 中的 SHA-256 哈希（不可逆），无任何个人标识
- `raw_events` 本身即为唯一事实来源；删除后派生表无法恢复原始数据

### 非诊断性输出

- 错误响应不泄露驱动名、SQL、连接串或堆栈
- 内部错误统一返回 `{"error": "internal error"}`，详细信息仅写服务端日志

## 测试

项目包含三类测试：

### 单元测试（`internal/service/`）

- 年龄计算（含时区跨日、闰年边界）
- 规则引擎（成人/青少年/儿童各限额及违规检测）
- **属性测试**（`testing/quick`）：重放确定性、事件顺序无关性、年龄非负、时长非负

### 集成测试（`internal/tests/`）

| 测试 | 验证点 |
|------|--------|
| `TestIdempotentEventIngestion` | 重复事件 accepted=1 duplicate=1 |
| `TestOutOfOrderAndLateEvents` | 乱序事件正确聚合 |
| `TestLateEventTriggersRecalculation` | 迟到事件生成新版本，旧版本不变 |
| `TestConcurrentEventUpload` | 20 协程并发同一事件，精确 1 accepted |
| `TestCrossDeviceConcurrentUpload` | 5 设备×10 事件并发，全部 accepted |
| `TestTimezoneCrossDay` | UTC 16:30 → Asia/Shanghai 次日正确分桶 |
| `TestTransactionRollback` | 非法事件导致事务回滚，无残留 |
| `TestFaultInjectionContextCancel` | 取消 context 后无数据写入 |
| `TestReplayConsistency` | 增量计算与全量重放结果一致 |
| `TestPropertyReplayDeterminism` | 多次重放结果完全一致 |
| `TestHardDeleteUser` | 删除后所有表清零，仅留哈希审计 |
| `TestNonDiagnosticErrorOutput` | 错误响应不泄露数据库内部信息 |
| `TestAdultDailyLimit` / `TestAdultSessionLimit` | 成人规则 |
| `TestTeenDailyLimit` / `TestTeenBedtimeViolation` | 青少年规则 |
| `TestChildSessionLimit` | 儿童规则 |

运行测试：

```bash
# 确保 PostgreSQL 运行中（docker compose up postgres 或本地 docker run）
GOTOOLCHAIN=local go test -race ./... -count=1 -timeout 120s
```

### 测试数据库

集成测试自动创建/销毁 `brainbreak_test` 数据库，使用与生产相同的迁移脚本。

## 项目结构

```
.
├── cmd/server/main.go              # 服务入口
├── internal/
│   ├── config/config.go            # 环境变量配置
│   ├── handler/http.go             # Gin 路由与请求处理
│   ├── models/models.go            # 数据结构
│   ├── migrations/                 # 嵌入的 SQL 迁移
│   │   ├── embed.go
│   │   └── 001_init.sql
│   ├── service/
│   │   ├── age.go                  # 时区感知年龄计算
│   │   ├── rules.go                # 规则引擎
│   │   ├── processor.go            # 事件处理、版本化重算、重放
│   │   ├── deletion.go             # 硬删除与授权撤回
│   │   └── service_test.go         # 单元 + 属性测试
│   ├── store/
│   │   ├── db.go                   # 连接池、迁移执行（文件/嵌入）
│   │   ├── users.go                # 用户存储
│   │   ├── experiments.go          # 实验存储
│   │   ├── events.go               # 幂等事件写入
│   │   ├── results.go              # 聚合与结果存储
│   │   └── auth.go                 # 授权与删除审计
│   └── tests/integration_test.go   # 集成 + 并发 + 故障注入测试
├── migrations/001_init.sql         # PostgreSQL schema（Docker init）
├── Dockerfile
├── docker-compose.yml
└── go.mod / go.sum
```
