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
	"strings"

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

// versionByte identifies the running build: "dev" locally; Cloud Build
// overwrites the file with the commit SHA before the image is built, so
// GET /version tells which deploy is live.
//
//go:embed version.txt
var versionByte []byte

// versionHandler serves the embedded build version as plain text.
func versionHandler(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, version)
	}
}

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
			// Sessions are kept after publishing (inspectable in the
			// Anthropic Console for prompt work); flip this env var to
			// "true" to go back to deleting them.
			DeleteSessions: env("GENERATION_DELETE_SESSIONS", "false") == "true",
		})
		go generator.Bootstrap(ctx)
		log.Info("generation: enabled", "model", env("GENERATION_MODEL", "claude-sonnet-5"))
	} else {
		log.Info("generation: disabled (ANTHROPIC_API_KEY not set)")
	}

	// ANTHROPIC_ADMIN_KEY (sk-ant-admin01-..., a different key type from
	// ANTHROPIC_API_KEY) unlocks /admin/costs and /admin/usage — real
	// billed dollars from Anthropic's Usage & Cost Admin API.
	adminKey := os.Getenv("ANTHROPIC_ADMIN_KEY")
	if adminKey == "" {
		log.Info("cost reporting: disabled (ANTHROPIC_ADMIN_KEY not set)")
	} else {
		log.Info("cost reporting: enabled (/admin/costs, /admin/usage)")
	}

	version := strings.TrimSpace(string(versionByte))
	handler, err := httpapi.New(httpapi.Config{
		Store:             st,
		BaseURL:           os.Getenv("BASE_URL"),
		AdminToken:        adminToken,
		Assets:            assetsFS,
		Logger:            log,
		Generator:         generator,
		AnthropicAdminKey: adminKey,
		Version:           version,
	})
	if err != nil {
		return err
	}

	// /version fronts the app handler: a deploy-tracking probe on the
	// Public Surface, like /healthz.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", versionHandler(version))
	mux.Handle("/", handler)

	addr := ":" + env("PORT", "8080")
	log.Info("listening", "addr", addr)
	return http.ListenAndServe(addr, mux)
}
