package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pingDSN 用短超时建立连接并 ping 一次，用于在 TestMain 早期失败而非用例中途跳过。
func pingDSN(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	return pool.Ping(ctx)
}

// TestMain 在执行所有集成测试之前确保 PostgreSQL 可用。
//
// 选择顺序：
//  1. 若环境变量 DATABASE_URL 已设置，直接使用（指向任意现成的 Postgres 实例，
//     每个测试会在自己的独立 schema 下运行，互不干扰）。
//  2. 否则通过本地 Docker CLI 启动一个一次性的 postgres:16 容器，
//     等待 pg_isready 就绪，并把连接串写入 DATABASE_URL，测试结束后自动销毁。
//
// 这样 `go test -v ./internal/integration -count=1` 不需要任何预先准备
// （只需本机安装 Docker）即可让全部用例针对真实 PostgreSQL 执行。
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run 把真实工作流放到非 TestMain 函数里，以便在 os.Exit 之前显式执行 cleanup。
func run(m *testing.M) int {
	if dsn := os.Getenv("DATABASE_URL"); dsn == "" {
		dsn, cleanup, err := startEphemeralPostgres()
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"integration tests require PostgreSQL. Set DATABASE_URL to a running instance, "+
					"or install Docker so one can be started automatically.\n  error: %v\n", err)
			return 1
		}
		os.Setenv("DATABASE_URL", dsn)
		defer cleanup()
	} else {
		// 探测连通性：连接不上直接失败，绝不跳过。
		if err := pingDSN(dsn); err != nil {
			fmt.Fprintf(os.Stderr, "DATABASE_URL set but unreachable: %v\n", err)
			return 1
		}
	}
	return m.Run()
}

// startEphemeralPostgres 通过 docker CLI 启动一个临时的 postgres:16 容器。
func startEphemeralPostgres() (string, func(), error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", nil, fmt.Errorf("docker executable not found in PATH")
	}

	name := "focus-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 拉取镜像（若本地没有）并启动。-d 让 docker run 在容器启动后返回。
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=postgres",
		"-e", "POSTGRES_DB=focus",
		"-p", "127.0.0.1::5432",
		"postgres:16")
	out, err := run.CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(string(out)))
	}

	cleanup := func() {
		c2, c2cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer c2cancel()
		_ = exec.CommandContext(c2, "docker", "rm", "-f", name).Run()
	}

	// 取出宿主机随机映射端口。
	hostPort, err := dockerHostPort(ctx, name)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	dsn := fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%s/focus?sslmode=disable", hostPort)

	// 等待 pg_isready，最多 60s。
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ready := exec.CommandContext(ctx, "docker", "exec", name, "pg_isready", "-U", "postgres")
		if out, err := ready.CombinedOutput(); err == nil {
			// 再 ping 一次确保网络侧也可达。
			if err := pingDSN(dsn); err == nil {
				return dsn, cleanup, nil
			} else {
				lastErr = err
			}
		} else {
			lastErr = fmt.Errorf("pg_isready: %s", strings.TrimSpace(string(out)))
		}
		time.Sleep(500 * time.Millisecond)
	}
	cleanup()
	return "", nil, fmt.Errorf("postgres did not become ready in time: %v", lastErr)
}

// dockerHostPort 通过 `docker port` 解析宿主端口。
func dockerHostPort(ctx context.Context, name string) (string, error) {
	var out []byte
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.CommandContext(ctx, "docker", "port", name, "5432/tcp")
		out, err = cmd.CombinedOutput()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return "", fmt.Errorf("docker port: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// 输出可能多行（IPv4 / IPv6），取形如 "127.0.0.1:PORT" 的最后一个并取端口段。
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	addr := strings.TrimSpace(lines[len(lines)-1])
	idx := strings.LastIndex(addr, ":")
	if idx < 0 || idx == len(addr)-1 {
		return "", fmt.Errorf("cannot parse docker port output: %q", string(out))
	}
	return addr[idx+1:], nil
}
