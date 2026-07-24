// Command focuslab runs the focus experiment event-processing service.
//
// Storage backend selection:
//   - If FOCUS_PG_DSN is set, the PostgreSQL store is used (migrations are
//     applied on startup).
//   - Otherwise an in-memory store is used, which is convenient for local
//     development and for environments without a database.
//
// The service uses only the standard library HTTP server plus pgx for Postgres.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"focuslab/internal/httpapi"
	"focuslab/internal/pgstore"
	"focuslab/internal/service"
	"focuslab/internal/store"
)

func main() {
	addr := envOr("FOCUS_ADDR", ":8080")
	dsn := os.Getenv("FOCUS_PG_DSN")

	ctx := context.Background()

	var st store.Store
	var closer func()
	if dsn != "" {
		pg, err := pgstore.Open(ctx, dsn)
		if err != nil {
			log.Fatalf("open postgres: %v", err)
		}
		if err := pg.Migrate(ctx); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		st = pg
		closer = pg.Close
		log.Printf("using postgres store")
	} else {
		st = store.NewMem()
		log.Printf("using in-memory store (set FOCUS_PG_DSN to use postgres)")
	}
	if closer != nil {
		defer closer()
	}

	svc := service.New(st, time.Now)
	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.NewServer(svc).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
