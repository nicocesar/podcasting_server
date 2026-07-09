// The podcasting server: private podcast feeds for AntennaPod, published
// by an external Generator. See README.md and CONTEXT.md.
package main

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/httpapi"
	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
	"github.com/nicocesar/podcasting_server/internal/store/gcpstore"
	"github.com/nicocesar/podcasting_server/internal/tts"
)

// The Public Surface pages ship inside the binary (ADR 0003).
//
//go:embed templates static
var assetsFS embed.FS

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(log *slog.Logger) error {
	ctx := context.Background()

	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		return fmt.Errorf("ADMIN_TOKEN must be set (guards user provisioning)")
	}

	var st store.Store
	var err error
	backend := env("STORAGE", "fs")
	switch backend {
	case "fs":
		dataDir := env("DATA_DIR", "./data")
		st, err = fsstore.New(dataDir)
		if err != nil {
			return err
		}
		log.Info("storage: filesystem (dev only)", "dir", dataDir)
	case "gcp":
		bucket := os.Getenv("GCS_BUCKET")
		if bucket == "" {
			return fmt.Errorf("GCS_BUCKET must be set when STORAGE=gcp")
		}
		st, err = gcpstore.New(ctx, os.Getenv("GCP_PROJECT"), bucket)
		if err != nil {
			return err
		}
		log.Info("storage: datastore + gcs", "bucket", bucket)
	default:
		return fmt.Errorf("unknown STORAGE %q (want fs or gcp)", backend)
	}

	// Built-in Generation (ADR 0009) turns on when an Anthropic key is
	// present; without one the /me/generate surface answers 503 and the
	// Dashboard hides it.
	var generator *generation.Runner
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		engines := []tts.Engine{tts.NewEdge()}
		if google, err := tts.NewGoogle(ctx); err != nil {
			log.Warn("generation: Google TTS unavailable, edge-tts only", "err", err)
		} else {
			engines = append(engines, google)
		}
		generator = generation.NewRunner(generation.Config{
			Store:   st,
			API:     generation.NewClient(key),
			Engines: engines,
			Model:   env("GENERATION_MODEL", "claude-sonnet-5"),
			Logger:  log,
		})
		go generator.Bootstrap(ctx)
		log.Info("generation: enabled", "model", env("GENERATION_MODEL", "claude-sonnet-5"))
	} else {
		log.Info("generation: disabled (ANTHROPIC_API_KEY not set)")
	}

	handler, err := httpapi.New(httpapi.Config{
		Store:      st,
		BaseURL:    os.Getenv("BASE_URL"),
		AdminToken: adminToken,
		Assets:     assetsFS,
		Logger:     log,
		Generator:  generator,
	})
	if err != nil {
		return err
	}

	addr := ":" + env("PORT", "8080")
	log.Info("listening", "addr", addr)
	return http.ListenAndServe(addr, handler)
}
