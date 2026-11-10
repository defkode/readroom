// readroom is a small self-hosted feed reader: one binary, one SQLite file.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
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
                READROOM_TOKEN_FILE  read the token from this file instead; created with a
                                   random token on first run
                READROOM_INTERVAL  how often feeds are polled (default 30m)
                READROOM_LOG       info or debug             (default info)
                READROOM_SUMMARY_MIN_CHARS  shortest article queued for an AI summary (default 1500)
  summarize   fill in missing article summaries with an LLM; see "readroom summarize -h"`)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "readroom:", err)
		os.Exit(1)
	}
}

// tokenFromFile reads the access token from path, creating a random one on first
// run so the secret never has to appear in a compose file.
func tokenFromFile(path string) (token string, created bool, err error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if token = strings.TrimSpace(string(data)); token == "" {
			return "", false, errors.New("token file is empty")
		}
		return token, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", false, err
	}
	token = hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, err
	}
	return token, true, os.WriteFile(path, []byte(token+"\n"), 0o600)
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
	if path := os.Getenv("READROOM_TOKEN_FILE"); token == "" && path != "" {
		var created bool
		if token, created, err = tokenFromFile(path); err != nil {
			return fmt.Errorf("READROOM_TOKEN_FILE: %w", err)
		}
		if created {
			log.Info("generated a new access token", "file", path)
		}
	}
	if token == "" {
		log.Warn("READROOM_TOKEN is not set: the UI and API are open to anyone who can reach this port")
	}
	app := server.New(st, fetcher, token, log)
	if v := os.Getenv("READROOM_SUMMARY_MIN_CHARS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return errors.New("READROOM_SUMMARY_MIN_CHARS must be a non-negative number")
		}
		app.SummaryMinChars = n
	}
	srv := &http.Server{
		Addr:              env("READROOM_ADDR", ":8090"),
		Handler:           app.Handler(),
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
