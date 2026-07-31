// The podcasting server: private podcast feeds for AntennaPod, published
// by an external Generator. See README.md and CONTEXT.md.
package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/coverart"
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

// versionByte is the release the repo calls this: hand-bumped, and the
// only one of the three build stamps that a person chose. Cloud Build
// used to overwrite it with the commit SHA, which meant the number in
// the file never reached production and there was no way to show the
// release and the commit at the same time. It ships as written now, and
// the commit arrives separately.
//
//go:embed version.txt
var versionByte []byte

// commit and builtAt are stamped in at link time — see the ldflags in
// the Dockerfile, fed by cloudbuild.yaml. Empty in a local `go build`,
// which is the honest answer there: a working tree is not a build.
var (
	commit  string
	builtAt string // RFC3339, UTC
)

// versionHandler serves the identifier of the running deploy as plain
// text. It answers the commit, not the release: this endpoint exists to
// tell which build is live, and two deploys of one release have to be
// distinguishable. Falls back to the release when there is no commit,
// so a local build still answers something.
func versionHandler(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, cmp.Or(commit, version))
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

// envInt reads a whole-number setting, warning rather than failing on
// nonsense. A typo in a knob whose zero value means "take the default"
// must not silently mean "budget zero, fire nothing".
func envInt(log *slog.Logger, key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Warn("config: ignoring unreadable value", "key", key, "value", raw, "using", fallback)
		return fallback
	}
	return n
}

// hostname reduces a base URL to the bare domain spoken in episode credits
// ("https://radio.example.com/" → "radio.example.com"). An empty or
// unparseable value yields "", which drops the host from the credit.
func hostname(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	// No scheme: url.Parse files everything under Path — take the authority.
	return strings.TrimSuffix(strings.SplitN(base, "/", 2)[0], "/")
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

	// One-shot maintenance subcommands run against the configured store
	// and exit — they never start the HTTP server.
	if len(os.Args) > 1 && os.Args[1] == "backfill-thumbs" {
		return backfillThumbs(ctx, log, st)
	}

	if err := seedCanon(ctx, log, st); err != nil {
		return err
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
			// Spoken in the episode credit ("...on radio.example.com...");
			// the same BASE_URL the feed links use, reduced to a bare domain.
			Host:   hostname(os.Getenv("BASE_URL")),
			Logger: log,
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
	// ANTHROPIC_WORKSPACE_ID scopes that reporting to the workspace this
	// server's API key belongs to. Without it the reports cover the whole
	// organization, which is only the same number when nothing else bills
	// to it — see ADR 0024.
	workspaceID := strings.TrimSpace(os.Getenv("ANTHROPIC_WORKSPACE_ID"))
	adminKey := os.Getenv("ANTHROPIC_ADMIN_KEY")
	if adminKey == "" {
		log.Info("cost reporting: disabled (ANTHROPIC_ADMIN_KEY not set)")
	} else if workspaceID == "" {
		log.Warn("cost reporting: enabled but org-wide (set ANTHROPIC_WORKSPACE_ID to scope it to this server)")
	} else {
		log.Info("cost reporting: enabled", "workspace", workspaceID)
	}

	// TICK_TOKEN is the credential Cloud Scheduler carries to POST /tick,
	// which is what fires Beats and resumes stalled Generations (ADR
	// 0028). Both the scheduler job and this variable are manual steps
	// outside the image-only deploy, so a deployment can be perfectly
	// healthy and have no clock at all — /admin says when the last Tick
	// landed, because nothing else would show it.
	tickToken := strings.TrimSpace(os.Getenv("TICK_TOKEN"))
	tickOpts := generation.TickOptions{
		LivenessWindow: time.Duration(envInt(log, "TICK_LIVENESS_HOURS", 0)) * time.Hour,
		BeatBudget:     envInt(log, "TICK_BEAT_BUDGET", 0),
	}
	if tickToken == "" {
		log.Info("tick: header credential disabled (TICK_TOKEN not set); POST /tick takes an admin session only")
	} else {
		log.Info("tick: enabled",
			"liveness_window", cmp.Or(tickOpts.LivenessWindow, generation.DefaultLivenessWindow),
			"beat_budget", cmp.Or(tickOpts.BeatBudget, generation.DefaultBeatBudget))
	}

	version := strings.TrimSpace(string(versionByte))
	handler, err := httpapi.New(httpapi.Config{
		Store:                st,
		BaseURL:              os.Getenv("BASE_URL"),
		AdminToken:           adminToken,
		SessionSecret:        sessionSecret,
		GoogleClientID:       googleID,
		GoogleClientSecret:   googleSecret,
		Assets:               assetsFS,
		Logger:               log,
		Generator:            generator,
		TickToken:            tickToken,
		Tick:                 tickOpts,
		AnthropicAdminKey:    adminKey,
		AnthropicWorkspaceID: workspaceID,
		ElevenLabsKey:        os.Getenv("ELEVENLABS_API_KEY"),
		Version:              version,
		Commit:               commit,
		BuiltAt:              builtAt,
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

// backfillThumbs regenerates the normalized full image and web thumbnail
// for every user who already has a cover, so covers uploaded before
// thumbnails existed get optimized in one pass. It reuses the upload
// path (coverart.Process + store.SetCover) and is safe to re-run.
// seedCanon gives a fresh install something to sort episodes into, so a
// new deployment is not dead on arrival (ADR 0017). It runs only when
// the canon is completely empty — an operator who has retired every
// strand meant it, and must not find them back after a restart. The
// four arrive Dormant: nothing may be aired into a strand until an
// admin has given it cover art on /admin/strands.
func seedCanon(ctx context.Context, log *slog.Logger, st store.Store) error {
	canon, err := st.ListStrands(ctx)
	if err != nil {
		return fmt.Errorf("read the strand canon: %w", err)
	}
	if len(canon) > 0 {
		return nil
	}
	seeds := store.SeedStrands()
	for _, s := range seeds {
		s.CreatedAt = time.Now().UTC()
		if err := st.PutStrand(ctx, s); err != nil {
			return fmt.Errorf("seed strand %q: %w", s.ID, err)
		}
	}
	log.Info("seeded an empty strand canon; upload cover art on /admin/strands to wake them",
		"strands", len(seeds))
	return nil
}

func backfillThumbs(ctx context.Context, log *slog.Logger, st store.Store) error {
	users, err := st.ListUsers(ctx)
	if err != nil {
		return err
	}
	var done, skipped int
	for _, u := range users {
		if u.CoverType == "" {
			skipped++
			continue
		}
		rc, ct, err := st.OpenCover(ctx, u.ID)
		if err != nil {
			log.Warn("backfill: cannot open cover", "user", u.ID, "err", err)
			skipped++
			continue
		}
		p, err := coverart.Process(rc, ct)
		rc.Close()
		if err != nil {
			log.Warn("backfill: cannot process cover", "user", u.ID, "err", err)
			skipped++
			continue
		}
		if err := st.SetCover(ctx, u.ID, p.FullType, bytes.NewReader(p.Full), bytes.NewReader(p.Thumb)); err != nil {
			return fmt.Errorf("backfill %s: %w", u.ID, err)
		}
		done++
		log.Info("backfill: thumbnail generated", "user", u.ID)
	}
	log.Info("backfill complete", "generated", done, "skipped", skipped)
	return nil
}
