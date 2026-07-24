package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"brainbreak-lab/internal/config"
	"brainbreak-lab/internal/handler"
	bmigrations "brainbreak-lab/internal/migrations"
	"brainbreak-lab/internal/store"

	"github.com/gin-gonic/gin"
)

func resolveMigrationsDir() string {
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), "migrations")
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	if info, err := os.Stat("migrations"); err == nil && info.IsDir() {
		return "migrations"
	}
	return ""
}

func main() {
	cfg := config.Load()

	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to connect database: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	migrationsDir := resolveMigrationsDir()
	if migrationsDir != "" {
		log.Printf("running migrations from directory: %s", migrationsDir)
		if err := store.RunMigrations(ctx, db, migrationsDir); err != nil {
			log.Fatalf("migration failed, aborting startup: %v", err)
		}
	} else {
		log.Println("running embedded migrations")
		if err := store.RunEmbeddedMigrations(ctx, db, bmigrations.FS); err != nil {
			log.Fatalf("embedded migration failed, aborting startup: %v", err)
		}
	}
	log.Println("migrations applied successfully")

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	h := handler.NewHandler(db)
	handler.RegisterRoutes(r, h)

	srv := &http.Server{
		Addr:    cfg.ServerAddr,
		Handler: r,
	}

	go func() {
		log.Printf("server listening on %s", cfg.ServerAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("forced shutdown: %v", err)
	}
	log.Println("server stopped")
}
