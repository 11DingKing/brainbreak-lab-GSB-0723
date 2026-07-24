# focuslab — 专注实验事件处理服务

一个用 Go 1.23 + PostgreSQL 实现的“专注实验事件处理服务”。接收四类带客户端序号与发生时间的专注事件，在**重复、乱序、延迟到达、跨设备并发上传**的情况下仍能**幂等**地生成**可重放**的实验结果；按用户时区**动态计算年龄**并执行成年人/青少年/儿童的防沉迷配额规则；提供实验创建、批量事件写入、结果查询、按版本重算、撤回授权与彻底删除接口。

仅使用标准库 HTTP + `pgx`（PostgreSQL 驱动），不依赖消息队列或外部 SaaS。

## 核心保证

| 要求 | 实现位置 |
|------|----------|
| 同一事件只计一次（幂等） | 事件自然幂等键 `(experiment_id, subject_id, device_id, client_seq)`；PG 侧 `UNIQUE` + `ON CONFLICT DO NOTHING`（[schema.sql](internal/pgstore/schema.sql)），内存侧按键去重 |
| 重复/乱序无关的结果 | 纯 domain 折叠 `Fold` 对事件做**规范化排序 + 去重**后计算，结果与到达顺序、重复无关（[fold.go](internal/domain/fold.go)） |
| 迟到事件触发结果修正 | `WriteEvents` 在同一事务内写事件并对**全量**事件重算，迟到事件改变 `digest` 并置 `result_corrected`（[usecases.go](internal/service/usecases.go)） |
| 可重放一致性 | `Digest` 为规范化事件集的 SHA-256；属性测试对 200 组随机数据做排列/重复不变性校验（[property_test.go](internal/domain/property_test.go)） |
| 按用户时区动态计算年龄 | `AgeInLocation` 将出生与当前时刻投影到 IANA 时区后比较（[policy.go](internal/domain/policy.go)） |
| 防沉迷规则 | 成人 每日 60m/单次 15m；青少年 每日 30m + 睡前 [22:00,23:00) 禁刷；儿童 单次 10m（[policy.go](internal/domain/policy.go)） |
| 删除后个人数据不可恢复 | 加密粉碎（crypto-shredding）：个人字段仅以 per-subject 密钥 AES-GCM 密文存储；彻底删除销毁密钥并级联清空派生表（[cryptoshred.go](internal/cryptoshred/cryptoshred.go)） |
| 事务回滚 | `WithTx` 全有或全无；故障注入测试强制回滚验证无残留（[fault.go](internal/store/fault.go)） |
| 非诊断性输出 | HTTP 错误只返回稳定 code + 固定文案，绝不回显内部错误、SQL、堆栈或个人数据（[server.go](internal/httpapi/server.go)） |

## 架构

```
cmd/focuslab            进程入口（选择 PG 或内存后端）
internal/domain         纯计算内核：事件模型、规范化排序、幂等折叠、年龄与配额规则（无 IO）
internal/cryptoshred    加密粉碎：per-subject 密钥 + AES-256-GCM
internal/store          持久化端口 + 内存事务实现 + 故障注入包装
internal/pgstore        PostgreSQL 实现（pgx）+ 迁移脚本
internal/service        用例编排（事务边界、迟到修正、版本重算、删除）
internal/httpapi        标准库 net/http 传输适配
```

domain 是纯函数内核——幂等、时区跨日、可重放都在这里保证，因此可以脱离数据库用属性/并发测试彻底验证。

## HTTP 接口

| 方法 & 路径 | 说明 |
|-------------|------|
| `POST /v1/experiments` | 创建实验与受试者（个人数据即时加密） |
| `POST /v1/experiments/{expID}/subjects/{subID}/events` | 批量写入事件（幂等），并原子重算结果 |
| `GET  /v1/experiments/{expID}/subjects/{subID}/result?version=` | 查询结果（缺省最新版本） |
| `POST /v1/experiments/{expID}/subjects/{subID}/recompute` | 按版本重算（`{"new_version":true}` 可升版保留旧结果） |
| `POST /v1/experiments/{expID}/subjects/{subID}/revoke` | 撤回授权（拒绝后续写入，保留既有结果） |
| `DELETE /v1/experiments/{expID}/subjects/{subID}` | 彻底删除（粉碎密钥 + 级联清空派生表） |

## 运行

```bash
# 内存后端（无需数据库，适合本地/演示）
go run ./cmd/focuslab

# PostgreSQL 后端（启动时自动迁移）
export FOCUS_PG_DSN='postgres://user:pass@localhost:5432/focuslab?sslmode=disable'
go run ./cmd/focuslab
```

环境变量：`FOCUS_ADDR`（默认 `:8080`）、`FOCUS_PG_DSN`（设置则用 PG，否则用内存）。

## 测试

```bash
go test ./...          # 单元 + 属性 + 并发 + 故障注入（PG 集成测试在未设 DSN 时自动跳过）
go test -race ./...    # 竞态检测
FOCUS_PG_DSN=... go test ./internal/pgstore/   # 运行真实 PG 集成测试
```

测试覆盖：
- **并发**：32 goroutine 跨设备重复上传，断言每个事件恰好计一次（[service_test.go](internal/service/service_test.go)）
- **属性**：200 组随机事件的排列/重复不变性 + 迟到收敛（[property_test.go](internal/domain/property_test.go)）
- **故障注入**：`SaveResult` 失败强制事务回滚，断言零残留（[service_test.go](internal/service/service_test.go)）
- **时区跨日**：本地日历日分桶、时区边界年龄（[fold_test.go](internal/domain/fold_test.go)）
- **非诊断输出**：错误响应不泄漏内部细节或个人数据（[server_test.go](internal/httpapi/server_test.go)）
- **加密粉碎**：删除后密文不可解、密钥不可重建（[cryptoshred_test.go](internal/cryptoshred/cryptoshred_test.go)）
