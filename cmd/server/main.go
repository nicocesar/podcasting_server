// The podcasting server: private podcast feeds for AntennaPod, published
// by an external Generator. See README.md and CONTEXT.md.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/httpapi"
	"github.com/nicocesar/podcasting_server/internal/music"
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

	// SESSION_SECRET signs the webapp session cookies (ADR 0010). In
	// production it must be stable across restarts and instances; for
	// local dev an ephemeral one is minted (sessions die on restart).
	sessionSecret := os.Getenv("SESSION_SECRET")

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
		if sessionSecret == "" {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			sessionSecret = hex.EncodeToString(b)
			log.Warn("SESSION_SECRET not set; using an ephemeral one (logins do not survive restarts)")
		}
	case "gcp":
		bucket := os.Getenv("GCS_BUCKET")
		if bucket == "" {
			return fmt.Errorf("GCS_BUCKET must be set when STORAGE=gcp")
		}
		if sessionSecret == "" {
			return fmt.Errorf("SESSION_SECRET must be set when STORAGE=gcp (signs login sessions)")
		}
		st, err = gcpstore.New(ctx, os.Getenv("GCP_PROJECT"), bucket)
		if err != nil {
			return err
		}
		log.Info("storage: datastore + gcs", "bucket", bucket)
	default:
		return fmt.Errorf("unknown STORAGE %q (want fs or gcp)", backend)
	}

	// GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET turn on "Sign in with
	// Google"; without them the webapp is password-only. Trimmed: a
	// stray space in the env value reaches Google verbatim inside the
	// auth URL and every sign-in fails as an invalid client.
	googleID := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID"))
	googleSecret := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET"))
	if googleID != "" && googleSecret != "" {
		log.Info("google sign-in: enabled")
	} else {
		log.Info("google sign-in: disabled (GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET not set)")
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
		// ElevenLabs goes last: billed per character, so it voices an
		// episode only when a Generation picks it in the provider
		// dropdown, never as the automatic first choice.
		if eleven, err := tts.NewElevenLabs(os.Getenv("ELEVENLABS_API_KEY")); err != nil {
			log.Info("generation: ElevenLabs TTS unavailable", "err", err)
		} else {
			engines = append(engines, eleven)
			log.Info("generation: ElevenLabs TTS enabled (opt-in per generation)")
		}
		// The same key also buys music composition, on a different
		// endpoint. Without it the ambient template is not merely
		// degraded but impossible, so it drops off the chooser entirely
		// rather than failing after an agent session has been spent.
		var composer generation.Composer
		if m, err := music.New(os.Getenv("ELEVENLABS_API_KEY")); err != nil {
			log.Info("generation: music composition unavailable, ambient program hidden", "err", err)
		} else {
			composer = m
			log.Info("generation: music composition enabled", "model", m.Model())
		}
		generator = generation.NewRunner(generation.Config{
			Store:   st,
			API:     generation.NewClient(key),
			Engines: engines,
			Music:   composer,
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
		Store:              st,
		BaseURL:            os.Getenv("BASE_URL"),
		AdminToken:         adminToken,
		SessionSecret:      sessionSecret,
		GoogleClientID:     googleID,
		GoogleClientSecret: googleSecret,
		Assets:             assetsFS,
		Logger:             log,
		Generator:          generator,
		AnthropicAdminKey:  adminKey,
		Version:            version,
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
