// readroom is a small self-hosted feed reader: one binary, one SQLite file.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"readroom/internal/fetch"
	"readroom/internal/server"
	"readroom/internal/store"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "summarize":
		err = summarize(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, `usage: readroom [serve|summarize]

  serve       run the reader (default). Configured through the environment:
                READROOM_ADDR      listen address            (default :8090)
                READROOM_DB        SQLite database path      (default ./readroom.db)
                READROOM_TOKEN     access token for UI + API (default: no authentication)
                READROOM_INTERVAL  how often feeds are polled (default 30m)
                READROOM_LOG       info or debug             (default info)
  summarize   fill in missing article summaries with an LLM; see "readroom summarize -h"`)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "readroom:", err)
		os.Exit(1)
	}
}

func serve() error {
	level := slog.LevelInfo
	if env("READROOM_LOG", "info") == "debug" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	interval, err := time.ParseDuration(env("READROOM_INTERVAL", "30m"))
	if err != nil || interval < time.Minute {
		return errors.New("READROOM_INTERVAL must be a duration of at least 1m, e.g. 30m")
	}
	dbPath := env("READROOM_DB", "readroom.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fetcher := fetch.New(st, interval, log)
	go fetcher.Run(ctx)

	token := os.Getenv("READROOM_TOKEN")
	if token == "" {
		log.Warn("READROOM_TOKEN is not set: the UI and API are open to anyone who can reach this port")
	}
	srv := &http.Server{
		Addr:              env("READROOM_ADDR", ":8090"),
		Handler:           server.New(st, fetcher, token, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Info("readroom listening", "addr", srv.Addr, "db", dbPath, "interval", interval.String())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
