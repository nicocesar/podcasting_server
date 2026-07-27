// Package httpapi is the HTTP surface of the podcasting server: the
// read-side endpoints AntennaPod consumes (feed, audio, cover), the
// write-side Publishing Contract + Management API under /me (see
// docs/adr/0001 and 0005), and the admin provisioning endpoints.
package httpapi

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"

	"github.com/nicocesar/podcasting_server/internal/audio"
	"github.com/nicocesar/podcasting_server/internal/coverart"
	"github.com/nicocesar/podcasting_server/internal/feed"
	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/store"
)

// maxUploadBytes caps write-request bodies. Cloud Run itself caps HTTP/1
// requests at 32 MiB; this is a backstop for local development.
const maxUploadBytes = 256 << 20

// inviteTTL bounds how long an Invite lives: one clock for both of the
// things it does, playing its Episode and admitting a User. ADR 0007
// chose 7 days when an Invite was only a door; a link that visibly still
// plays should still open, so ADR 0014 widened it to 30 and accepted the
// longer-lived door as the price of one comprehensible rule.
const inviteTTL = 30 * 24 * time.Hour

type Config struct {
	Store store.Store
	// BaseURL overrides the external base URL used in feed links. When
	// empty, it is derived per-request from Host and X-Forwarded-Proto,
	// which is correct behind Cloud Run.
	BaseURL string
	// AdminToken guards the /admin endpoints (Authorization: Bearer).
	// Users authenticate with their own credentials (ADR 0005).
	AdminToken string
	// SessionSecret signs session cookies and OAuth state (ADR 0010).
	// Rotating it logs every browser out.
	SessionSecret string
	// GoogleClientID/GoogleClientSecret enable "Sign in with Google"
	// (OIDC code flow). Leave both empty to run password-only: the
	// Google buttons simply do not render.
	GoogleClientID     string
	GoogleClientSecret string
	// GoogleTokenURL overrides Google's token endpoint (tests only).
	GoogleTokenURL string
	// Assets holds the "templates" and "static" directories for the
	// Public Surface pages (cmd/server embeds and passes them).
	Assets fs.FS
	Logger *slog.Logger
	// Generator runs built-in Generations (ADR 0009). Nil disables the
	// /me/generate surface (503) and hides it from the Dashboard.
	Generator *generation.Runner
	// AnthropicAdminKey (sk-ant-admin01-...) unlocks GET /admin/costs and
	// GET /admin/usage, which proxy Anthropic's Usage & Cost Admin API —
	// the real-dollar counterpart of the per-Generation meters. Empty →
	// those endpoints answer 503.
	AnthropicAdminKey string
	// AnthropicAdminBaseURL overrides the Admin API host (tests only).
	AnthropicAdminBaseURL string
	// Version is the running build (cmd/server embeds version.txt); the
	// Dashboard shows it so users can tell which release they are on.
	Version string
}

type server struct {
	store         store.Store
	baseURL       string
	adminHash     [32]byte
	sessionSecret []byte
	google        *googleOIDC // nil: password-only
	log           *slog.Logger
	generator     *generation.Runner
	adminAPI      *anthropicAdmin
	version       string
	assetVersion  string // content hash of style.css; cache-busts the stylesheet URL

	tmplHome       *template.Template
	tmplUser       *template.Template
	tmplEpisode    *template.Template
	tmplLogin      *template.Template
	tmplInvite     *template.Template
	tmplWelcome    *template.Template
	tmplDashboard  *template.Template
	tmplNotFound   *template.Template
	tmplPrograms   *template.Template
	tmplGenerate   *template.Template
	tmplGeneration *template.Template
	tmplBeats      *template.Template
	tmplSettings   *template.Template

	tmplStrands *template.Template
	tmplStrand  *template.Template

	tmplAdminGeneration *template.Template
	tmplAdminStrands    *template.Template
	tmplAdmin           *template.Template
}

func New(cfg Config) (http.Handler, error) {
	if cfg.AdminToken == "" {
		return nil, errors.New("httpapi: AdminToken must be set")
	}
	if cfg.SessionSecret == "" {
		return nil, errors.New("httpapi: SessionSecret must be set")
	}
	s := &server{
		store:         cfg.Store,
		baseURL:       strings.TrimSuffix(cfg.BaseURL, "/"),
		adminHash:     sha256.Sum256([]byte(cfg.AdminToken)),
		sessionSecret: []byte(cfg.SessionSecret),
		log:           cfg.Logger,
		generator:     cfg.Generator,
		adminAPI:      newAnthropicAdmin(cfg.AnthropicAdminKey, cfg.AnthropicAdminBaseURL),
		version:       cfg.Version,
	}
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		s.google = &googleOIDC{
			clientID:     cfg.GoogleClientID,
			clientSecret: cfg.GoogleClientSecret,
			tokenURL:     cfg.GoogleTokenURL,
		}
	}
	if s.log == nil {
		s.log = slog.Default()
	}

	// Asset URLs carry a content hash so a deploy invalidates cached CSS
	// and JS immediately, while /static keeps its long max-age. One hash
	// covers both files: a version that changes slightly too often costs
	// one extra fetch, while a stale player is a bug report.
	s.assetVersion = "dev"
	h := sha256.New()
	hashed := false
	for _, name := range []string{"static/style.css", "static/player.js", "static/beat.js", "static/coverart.js"} {
		if b, err := fs.ReadFile(cfg.Assets, name); err == nil {
			h.Write(b)
			hashed = true
		}
	}
	if hashed {
		s.assetVersion = hex.EncodeToString(h.Sum(nil)[:4])
	}

	// Each page is layout + its content template (+ shared fragments).
	for _, p := range []struct {
		dst   **template.Template
		files []string
	}{
		{&s.tmplHome, []string{"templates/layout.html", "templates/home.html"}},
		{&s.tmplUser, []string{"templates/layout.html", "templates/user.html", "templates/fragments/*.html"}},
		{&s.tmplEpisode, []string{"templates/layout.html", "templates/episode.html", "templates/fragments/*.html"}},
		{&s.tmplLogin, []string{"templates/layout.html", "templates/login.html"}},
		{&s.tmplInvite, []string{"templates/layout.html", "templates/invite.html", "templates/fragments/*.html"}},
		{&s.tmplWelcome, []string{"templates/layout.html", "templates/welcome.html", "templates/fragments/*.html"}},
		{&s.tmplDashboard, []string{"templates/layout.html", "templates/dashboard.html", "templates/fragments/*.html"}},
		{&s.tmplNotFound, []string{"templates/layout.html", "templates/notfound.html"}},
		{&s.tmplPrograms, []string{"templates/layout.html", "templates/programs.html"}},
		{&s.tmplGenerate, []string{"templates/layout.html", "templates/generate.html"}},
		{&s.tmplGeneration, []string{"templates/layout.html", "templates/generation.html"}},
		{&s.tmplBeats, []string{"templates/layout.html", "templates/beats.html"}},
		{&s.tmplSettings, []string{"templates/layout.html", "templates/settings.html"}},
		{&s.tmplStrands, []string{"templates/layout.html", "templates/strands.html"}},
		{&s.tmplStrand, []string{"templates/layout.html", "templates/strand.html", "templates/fragments/*.html"}},
		{&s.tmplAdminGeneration, []string{"templates/layout.html", "templates/admin_generation.html"}},
		{&s.tmplAdminStrands, []string{"templates/layout.html", "templates/admin_strands.html"}},
		{&s.tmplAdmin, []string{"templates/layout.html", "templates/admin.html"}},
	} {
		t, err := template.New("page").Funcs(template.FuncMap{
			"assetv": func() string { return s.assetVersion },
		}).ParseFS(cfg.Assets, p.files...)
		if err != nil {
			return nil, fmt.Errorf("parse templates: %w", err)
		}
		*p.dst = t
	}
	static, err := fs.Sub(cfg.Assets, "static")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	// Public Surface (no auth; ADR 0003/0005): the landing page and
	// static assets. Nothing about a User is enumerable. The catch-all
	// makes every unmatched path a styled 404.
	mux.HandleFunc("GET /{$}", s.handleHome)
	// The Redemption page: the only way to join (ADR 0007). Invalid,
	// expired, and redeemed tokens are indistinguishable from any other
	// 404.
	// An Invite's token is a capability like any other: it plays one
	// Episode and admits one User (ADR 0014), so its namespace gets the
	// same no-referrer treatment as /f/.
	mux.HandleFunc("GET /invites/{token}", s.guest(s.handleInvitePage))
	mux.HandleFunc("POST /invites/{token}", s.guest(s.handleRedeem))
	mux.HandleFunc("GET /invites/{token}/audio.mp3", s.guest(s.handleInviteAudio))
	mux.HandleFunc("GET /invites/{token}/cover", s.guest(s.handleInviteCover))
	// The public side (ADR 0018): no capability at all. Literal segments
	// beat the wildcard, so feed.xml and cover never reach the audio
	// handler.
	mux.HandleFunc("GET /strands", s.handleStrandsIndex)
	mux.HandleFunc("GET /strands/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/strands", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /strands/{strand}", s.handleStrandPage)
	mux.HandleFunc("GET /strands/{strand}/feed.xml", s.handleStrandFeed)
	mux.HandleFunc("GET /strands/{strand}/cover", s.handleStrandCover)
	mux.HandleFunc("GET /strands/{strand}/{file}", s.handleStrandAudio)
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheControl("public, max-age=86400", http.FileServerFS(static))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.renderNotFound(w, r)
	})

	// Webapp login (ADR 0010). The login page is Public Surface; the
	// session it creates is the browser's credential for /me.
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	if s.google != nil {
		mux.HandleFunc("GET /auth/google", s.handleGoogleStart)
		mux.HandleFunc("GET /auth/google/callback", s.handleGoogleCallback)
	}

	// Read side (ADR 0008): the Feed Token capability namespace. The
	// URL is the credential — podcast clients never see an auth dialog.
	mux.HandleFunc("GET /f/{token}", s.feed(s.handleFeedLanding))
	mux.HandleFunc("GET /f/{token}/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/f/"+r.PathValue("token"), http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /f/{token}/feed.xml", s.feed(s.handleFeed))
	mux.HandleFunc("GET /f/{token}/cover", s.feed(s.handleCover))
	mux.HandleFunc("GET /f/{token}/qr.png", s.feed(s.handleQR))
	mux.HandleFunc("GET /f/{token}/{owner}/{file}", s.feed(s.handleEpisodeFile))

	// Publishing Contract + Management API: a Bearer API Key or a
	// session cookie (ADR 0010). Everything is scoped to the caller:
	// publishing into someone else's feed is inexpressible (ADR 0005).
	mux.HandleFunc("GET /me", s.auth(s.handleGetMe))
	mux.HandleFunc("GET /me/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/me", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /me/users", s.auth(s.handleSearchUsers))
	mux.HandleFunc("PUT /me", s.auth(s.handleUpdateMe))
	mux.HandleFunc("PUT /me/image", s.auth(s.handleSetCover))
	// The same Cover Art the feed serves, addressed under the session so
	// signed-in pages need no capability in their markup.
	mux.HandleFunc("GET /me/image", s.session(s.handleMyCover))
	mux.HandleFunc("GET /me/feed", s.auth(s.handleListFeed))
	mux.HandleFunc("GET /me/episodes", s.auth(s.handleListEpisodes))
	// The signed-in listening surface: same Episode Page and enclosure as
	// /f/{token}/{owner}/{file}, without a capability in the URL.
	mux.HandleFunc("GET /me/episodes/{owner}/{file}", s.session(s.handleMyEpisode))
	mux.HandleFunc("PUT /me/episodes/{slug}", s.auth(s.handlePublish))
	mux.HandleFunc("DELETE /me/episodes/{slug}", s.auth(s.handleDeleteEpisode))
	mux.HandleFunc("POST /me/feed/{owner}/{slug}/share", s.auth(s.handleShare))
	mux.HandleFunc("DELETE /me/feed/{owner}/{slug}", s.auth(s.handleRemoveShare))
	mux.HandleFunc("PUT /me/blocks/{user}", s.auth(s.handleSetList))
	mux.HandleFunc("DELETE /me/blocks/{user}", s.auth(s.handleSetList))
	mux.HandleFunc("PUT /me/mutes/{user}", s.auth(s.handleSetList))
	mux.HandleFunc("DELETE /me/mutes/{user}", s.auth(s.handleSetList))
	// The public side. Session-only: going public, and putting your name
	// to someone else's episode, are decisions a person makes in a
	// browser — never something a leaked API Key can do (ADR 0018).
	mux.HandleFunc("POST /me/episodes/{slug}/air", s.session(s.handleAir))
	mux.HandleFunc("POST /me/episodes/{slug}/unair", s.session(s.handleUnair))
	mux.HandleFunc("POST /me/vouches/{airing}", s.session(s.handleVouch))
	mux.HandleFunc("DELETE /me/vouches/{airing}", s.session(s.handleUnvouch))
	// Browsers cannot send DELETE from a form, and these controls live on
	// a page, so each removal has a POST spelling too.
	mux.HandleFunc("POST /me/vouches/{airing}/remove", s.session(s.handleUnvouch))
	mux.HandleFunc("POST /me/follows/{strand}/unfollow", s.session(s.handleUnfollow))
	mux.HandleFunc("PUT /me/follows/{strand}", s.session(s.handleFollow))
	mux.HandleFunc("POST /me/follows/{strand}", s.session(s.handleFollow))
	mux.HandleFunc("DELETE /me/follows/{strand}", s.session(s.handleUnfollow))
	mux.HandleFunc("POST /me/invites", s.auth(s.handleCreateInvite))
	mux.HandleFunc("GET /me/invites", s.auth(s.handleListInvites))
	mux.HandleFunc("DELETE /me/invites/{token}", s.auth(s.handleRevokeInvite))

	// Credential Management: session-only by construction, so a leaked
	// API Key can never widen itself, change the Login, or move the
	// Feed Token (CONTEXT.md "Credential Management").
	mux.HandleFunc("GET /me/settings", s.session(s.handleGetSettings))
	mux.HandleFunc("POST /me/feed-token", s.session(s.handleResetFeedToken))
	mux.HandleFunc("GET /me/api-keys", s.session(s.handleListAPIKeys))
	mux.HandleFunc("POST /me/api-keys", s.session(s.handleMintAPIKey))
	mux.HandleFunc("DELETE /me/api-keys/{keyid}", s.session(s.handleRevokeAPIKey))
	mux.HandleFunc("POST /me/password", s.session(s.handleSetPassword))
	mux.HandleFunc("POST /me/google/unlink", s.session(s.handleGoogleUnlink))
	mux.HandleFunc("POST /me/logout-everywhere", s.session(s.handleLogoutEverywhere))

	// Built-in Generation (ADR 0009): topic in, Episode in the caller's
	// own feed out, with an observable in-between. /me/generate is the
	// program chooser; each template has its own form page. The bare
	// POST stays as the news alias for JSON/API clients that predate
	// templates.
	mux.HandleFunc("GET /me/generate", s.auth(s.generating(s.handleGenerateChooser)))
	mux.HandleFunc("POST /me/generate", s.auth(s.generating(s.handleGenerateStart)))
	mux.HandleFunc("GET /me/generate/{template}", s.auth(s.generating(s.handleGeneratePage)))
	mux.HandleFunc("POST /me/generate/{template}", s.auth(s.generating(s.handleGenerateStart)))
	mux.HandleFunc("GET /me/generations/{id}", s.auth(s.generating(s.handleGeneration)))
	mux.HandleFunc("POST /me/generations/{id}/retry", s.auth(s.generating(s.handleGenerationRetry)))
	// Cast extraction backfill for a story episode the checkbox missed.
	mux.HandleFunc("POST /me/episodes/{slug}/characters", s.auth(s.generating(s.handleEpisodeCharacters)))

	// Beats (ADR 0016): the Generations a User asked to keep happening.
	// Session-only — a Beat spends money unattended, so a leaked API Key
	// must not be able to leave one running.
	mux.HandleFunc("GET /me/beats", s.session(s.generating(s.handleBeats)))
	mux.HandleFunc("GET /me/beats/{id}/edit", s.session(s.generating(s.handleBeatEdit)))
	mux.HandleFunc("POST /me/beats/{id}", s.session(s.generating(s.handleBeatUpdate)))
	mux.HandleFunc("POST /me/beats/{id}/pause", s.session(s.generating(s.handleBeatPause)))
	mux.HandleFunc("POST /me/beats/{id}/resume", s.session(s.generating(s.handleBeatResume)))
	mux.HandleFunc("POST /me/beats/{id}/cancel", s.session(s.generating(s.handleBeatCancel)))

	// Admin, on two credentials by design.
	//
	// ADMIN_TOKEN (header-only, break-glass): provisioning and credential
	// recovery (ADR 0007) plus appointing admins. These must work when
	// normal login cannot — on a fresh deployment there are no users at
	// all, so a session-authenticated "create the first user" is
	// unreachable by construction. Their whole purpose is to be the path
	// that survives a lockout, so they stay on the token.
	// Provisioning and appointment take either credential, so an existing
	// admin can do them from the webapp; the token still works, and is
	// the only thing that works before the first admin exists.
	mux.HandleFunc("PUT /admin/users/{user}", s.adminOrToken(s.handleCreateUser))
	mux.HandleFunc("POST /admin/users/{user}/admin", s.adminOrToken(s.handleSetAdmin))
	// Deleting a user and resetting anyone's password stay token-only.
	// Both are account takeover in one call, and the token lives in Secret
	// Manager and never touches a browser — so a stolen session cookie
	// cannot reach them.
	mux.HandleFunc("DELETE /admin/users/{user}", s.admin(s.handleDeleteUser))
	mux.HandleFunc("POST /admin/users/{user}/password-reset", s.admin(s.handlePasswordReset))

	// A logged-in admin (store.User.Admin): the reporting surfaces. These
	// are for reading, in a browser, which the header-only token cannot
	// do. Real billed dollars come from Anthropic's Usage & Cost Admin
	// API; the trace is the per-Generation execution record.
	// Listing users takes the token too: it is how an operator finds the
	// id to promote, and before the first admin exists the token is the
	// only credential there is.
	// The index the chrome's Admin link points at. Reading only: every
	// surface it names guards itself.
	mux.HandleFunc("GET /admin", s.adminUser(ignoreUser(s.handleAdminIndex)))
	mux.HandleFunc("GET /admin/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /admin/users", s.adminOrToken(s.handleListUsers))
	mux.HandleFunc("GET /admin/costs", s.adminUser(ignoreUser(s.handleAdminCosts)))
	mux.HandleFunc("GET /admin/costs/episodes", s.adminUser(ignoreUser(s.handleAdminEpisodeCosts)))
	mux.HandleFunc("GET /admin/usage", s.adminUser(ignoreUser(s.handleAdminUsage)))
	mux.HandleFunc("GET /admin/generations/{user}/{id}", s.adminUser(s.handleAdminGeneration))
	// The takedown (ADR 0018): the smallest power that works — the
	// Episode survives in its Owner's feed, only the publicness stops.
	mux.HandleFunc("POST /admin/airings/{airing}/unair", s.adminUser(s.handleAdminUnair))
	mux.HandleFunc("GET /admin/strands", s.adminUser(s.handleAdminStrands))
	mux.HandleFunc("POST /admin/strands", s.adminUser(s.handleAdminStrandCreate))
	mux.HandleFunc("POST /admin/strands/{strand}", s.adminUser(s.handleAdminStrandUpdate))
	mux.HandleFunc("GET /admin/strands/cover/preview", s.adminUser(s.handleAdminCoverPreview))
	mux.HandleFunc("POST /admin/strands/{strand}/cover", s.adminUser(s.handleAdminStrandCover))
	mux.HandleFunc("POST /admin/strands/{strand}/cover/generate", s.adminUser(s.handleAdminStrandGenerateCover))
	mux.HandleFunc("POST /admin/strands/{strand}/{action}", s.adminUser(s.handleAdminStrandAction))

	return s.logged(mux), nil
}

// --- middleware ---

// upperFirst capitalises the first byte, turning a lower-case rule
// message into a sentence. The messages are ASCII, so a byte is enough.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func hashEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 && a != ""
}

type authedHandler func(w http.ResponseWriter, r *http.Request, u store.User)

// ignoreUser adapts a plain handler to authedHandler, for admin endpoints
// that need the caller to *be* an admin but do not care which one.
func ignoreUser(h http.HandlerFunc) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, _ store.User) { h(w, r) }
}

// guest wraps the Invite namespace, whose URLs are capabilities held by
// people with no account. Same reasoning as feed: a link followed out of
// one of these pages must not carry the token in a Referer header.
func (s *server) guest(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		h(w, r.WithContext(withCapabilityScope(r.Context())))
	}
}

// feed resolves the {token} path segment to its User. An unknown token
// is a plain 404: capability URLs reveal nothing, valid or not.
//
// Every response here has the Feed Token in its own URL, so it also
// carries Referrer-Policy: no-referrer. Without it, any link a user
// follows out of one of these pages — a link inside an episode
// description, say — would hand the whole capability to the
// destination site in the Referer header.
func (s *server) feed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		u, err := s.store.GetUserByFeedToken(r.Context(), r.PathValue("token"))
		if err != nil {
			s.fail(w, err)
			return
		}
		// The bar stays public here even for a signed-in reader: this
		// URL is the whole credential and works for whoever holds it,
		// so it must not offer one particular member's navigation.
		h(w, r.WithContext(withCapabilityScope(r.Context())), u)
	}
}

// hasAdminToken reports whether the request carries ADMIN_TOKEN, compared
// as a digest in constant time.
func (s *server) hasAdminToken(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], s.adminHash[:]) == 1
}

func (s *server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.hasAdminToken(r) {
			h(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// adminOrToken admits ADMIN_TOKEN or a logged-in admin. The token path is
// what makes a fresh deployment bootstrappable — it has to work when no
// admin exists yet — and the session path is what lets an existing admin
// provision from the browser instead of reaching for a shared secret in a
// terminal.
//
// Deliberately s.session and not s.auth: this grants user provisioning
// and admin appointment, which are credential management. ADR 0010 keeps
// those out of an API Key's reach, and that matters more here than
// elsewhere — a leaked key that could appoint an admin would be a
// privilege escalation path, not just an over-broad read.
func (s *server) adminOrToken(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.hasAdminToken(r) {
			h(w, r)
			return
		}
		s.session(func(w http.ResponseWriter, r *http.Request, u store.User) {
			if !u.Admin {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		})(w, r)
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

// --- feed assembly ---

// feedEntry is one item in a Personal Feed: an Episode plus, when it was
// shared into this feed, its provenance (ADR 0006).
type feedEntry struct {
	store.Episode
	SharerID string     `json:"sharer,omitempty"`
	SharedAt *time.Time `json:"shared_at,omitempty"`

	// Strand and AiringID are set on an entry a Follow delivered: the
	// third kind of reference a feed holds (ADR 0019). They also decide
	// the enclosure — a delivered Episode is public, so its audio is
	// addressed on its Strand and not inside this reader's Feed Token.
	Strand   string `json:"strand,omitempty"`
	AiringID string `json:"airing,omitempty"`
	// Author is the Owner's feed title, carried only on delivered
	// entries because nothing else in a Personal Feed needs it.
	Author string `json:"author,omitempty"`
}

// Delivered reports whether a Follow put this entry in the feed rather
// than the reader making it or someone sharing it.
func (e feedEntry) Delivered() bool { return e.AiringID != "" }

// feedEntries assembles u's Personal Feed: own episodes, shared-in
// references, and whatever their Follows deliver, muted owners hidden,
// newest-first. from ("" = all, "me", or an owner ID) and filter
// ("" = all, "mine", "shared", "followed") are the Feed Variant
// parameters (ADR 0005).
func (s *server) feedEntries(r *http.Request, u store.User, from, filter string) ([]feedEntry, error) {
	if from == "me" {
		from = u.ID
	}
	entries := []feedEntry{}

	if filter != "shared" && filter != "followed" && (from == "" || from == u.ID) {
		own, err := s.store.ListEpisodes(r.Context(), u.ID)
		if err != nil {
			return nil, err
		}
		for _, ep := range own {
			entries = append(entries, feedEntry{Episode: ep})
		}
	}

	if filter != "mine" && filter != "followed" && from != u.ID {
		shares, err := s.store.ListShares(r.Context(), u.ID)
		if err != nil {
			return nil, err
		}
		for _, sh := range shares {
			if u.Muted(sh.OwnerID) {
				continue
			}
			if from != "" && sh.OwnerID != from {
				continue
			}
			ep, err := s.store.GetEpisode(r.Context(), sh.OwnerID, sh.Slug)
			if errors.Is(err, store.ErrNotFound) {
				continue // deleted since; the reference is dead
			}
			if err != nil {
				return nil, err
			}
			sharedAt := sh.SharedAt
			entries = append(entries, feedEntry{Episode: ep, SharerID: sh.SharerID, SharedAt: &sharedAt})
		}
	}

	// What the user's Follows bring in (ADR 0019). Last, so the dedupe
	// below can prefer everything already here: an explicit Share was
	// somebody's decision to send you this, and the credit line should
	// say so rather than crediting a strand.
	if filter != "mine" && filter != "shared" {
		delivered, err := s.deliveredEpisodes(r, u)
		if err != nil {
			return nil, err
		}
		have := make(map[string]bool, len(entries))
		for _, e := range entries {
			have[e.OwnerID+"/"+e.Slug] = true
		}
		for _, d := range delivered {
			if have[d.Episode.OwnerID+"/"+d.Episode.Slug] {
				continue
			}
			if from != "" && d.Episode.OwnerID != from {
				continue
			}
			entries = append(entries, feedEntry{
				Episode:  d.Episode,
				Strand:   d.Airing.Strand,
				AiringID: d.Airing.ID,
				Author:   d.Author,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].PublishedAt.Equal(entries[j].PublishedAt) {
			return entries[i].PublishedAt.After(entries[j].PublishedAt)
		}
		return entries[i].OwnerID+"/"+entries[i].Slug > entries[j].OwnerID+"/"+entries[j].Slug
	})
	return entries, nil
}

// --- read side ---

func (s *server) base(r *http.Request) string {
	if s.baseURL != "" {
		return s.baseURL
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
	}
	return proto + "://" + r.Host
}

// feedURL is the user's subscribe URL — the capability itself.
func (s *server) feedURL(r *http.Request, u store.User) string {
	return s.base(r) + "/f/" + u.FeedToken + "/feed.xml"
}

// deepLink is the one-tap AntennaPod subscribe URL.
func deepLink(feedURL string) string {
	return "https://antennapod.org/deeplink/subscribe?url=" + url.QueryEscape(feedURL)
}

func (s *server) handleFeed(w http.ResponseWriter, r *http.Request, u store.User) {
	entries, err := s.feedEntries(r, u, r.URL.Query().Get("from"), r.URL.Query().Get("filter"))
	if err != nil {
		s.fail(w, err)
		return
	}
	base := s.base(r)
	items := make([]feed.Item, len(entries))
	for i, e := range entries {
		it := feed.Item{Episode: e.Episode, Author: e.OwnerID}
		if e.Delivered() {
			// Delivered by a Follow: the audio is public, so it is
			// addressed on its Strand rather than inside this reader's
			// Feed Token, and credited to its Owner's feed title the
			// way the Strand does (ADR 0018/0019).
			it.EnclosureURL = feed.StrandEnclosure(base, e.Strand, e.AiringID)
			it.Author = e.Author
		} else {
			it.EnclosureURL = feed.FeedTokenEnclosure(base, u.FeedToken, e.Episode)
		}
		items[i] = it
	}
	body, err := feed.RSS(u, items, base)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write(body)

	// The heartbeat that matters (ADR 0016): a podcast client polling for
	// new audio is the one sign of life that arrives while its owner is
	// asleep. Fired after the response, so the feed is never held up —
	// which also means this poll gets yesterday's Episodes and the next
	// one gets what this heartbeat starts.
	s.heartbeat(u)
}

// handleEpisodeFile splits one address into two representations of the
// same Episode: `{slug}.mp3` is the enclosure a podcast client fetches,
// and the bare `{slug}` is the Episode Page a browser reads (ADR 0013).
// The suffix is the only thing separating them, so it stays strict.
func (s *server) handleEpisodeFile(w http.ResponseWriter, r *http.Request, u store.User) {
	if strings.HasSuffix(r.PathValue("file"), ".mp3") {
		s.handleAudio(w, r, u)
		return
	}
	s.handleEpisodePage(w, r, u)
}

// visibleEpisode resolves an Episode inside the feed's capability
// namespace: the feed's owner may reach their own Episodes and any
// shared into their feed; everything else does not exist.
func (s *server) visibleEpisode(r *http.Request, u store.User, ownerID, slug string) (store.Episode, error) {
	if u.ID != ownerID {
		if _, err := s.store.GetShare(r.Context(), u.ID, ownerID, slug); err != nil {
			return store.Episode{}, err
		}
	}
	return s.store.GetEpisode(r.Context(), ownerID, slug)
}

// handleEpisodePage renders one Episode as HTML: cover, description, and
// an inline Player. Its URL contains the Feed Token, so it is a place to
// listen and deliberately not a share link — passing it on would pass on
// the whole feed. Sharing stays Share-to-username or Invite (ADR 0013).
func (s *server) handleEpisodePage(w http.ResponseWriter, r *http.Request, u store.User) {
	s.episodePage(w, r, u, r.PathValue("owner"), r.PathValue("file"), false)
}

// episodePage renders the Episode Page on either of its two addresses.
// session picks which one: a signed-in browser gets URLs under /me,
// which keeps the Feed Token out of the address bar entirely, while a
// token holder gets capability URLs under /f/{token}.
func (s *server) episodePage(w http.ResponseWriter, r *http.Request, u store.User, ownerID, slug string, session bool) {
	if !store.ValidID(ownerID) || !store.ValidID(slug) {
		http.NotFound(w, r)
		return
	}
	ep, err := s.visibleEpisode(r, u, ownerID, slug)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The cover is the feed's, not the Episode owner's: the RSS channel
	// already presents shared Episodes under this feed's art, and no
	// route exposes another user's cover inside this token anyway.
	cover := coverURL(u)
	if session {
		cover = sessionCoverURL(u)
	}
	data := struct {
		Episode store.Episode
		// Published, not Aired: since ADR 0018 airing means going
		// public, and this is only when the episode was made.
		Published string
		Duration  string
		CoverURL  string
		AudioURL  string
		Session   bool
		Player    playerView
		subscribeBox
	}{
		Episode:      ep,
		Published:    relativeDate(ep.PublishedAt),
		Duration:     humanDuration(ep.DurationSec),
		CoverURL:     cover,
		AudioURL:     audioURL(u, ep, session),
		Session:      session,
		Player:       playerFor(u, ep, session),
		subscribeBox: s.subscribeBox(r, u),
	}
	s.render(w, r, http.StatusOK, s.tmplEpisode, data)
}

// handleMyEpisode is the signed-in twin of the capability routes: the
// same Episode Page and the same enclosure, addressed under /me and
// authorised by the session cookie the browser already has. A logged-in
// listener therefore never has the Feed Token in their address bar, so
// there is no full-feed capability to leak by copying the URL.
func (s *server) handleMyEpisode(w http.ResponseWriter, r *http.Request, u store.User) {
	ownerID := r.PathValue("owner")
	if slug, ok := strings.CutSuffix(r.PathValue("file"), ".mp3"); ok {
		if !store.ValidID(ownerID) || !store.ValidID(slug) {
			http.NotFound(w, r)
			return
		}
		if _, err := s.visibleEpisode(r, u, ownerID, slug); err != nil {
			s.fail(w, err)
			return
		}
		s.serveAudio(w, r, ownerID, slug)
		return
	}
	s.episodePage(w, r, u, ownerID, r.PathValue("file"), true)
}

// handleAudio serves an enclosure inside the feed's capability
// namespace, under the same visibility rule as the Episode Page.
func (s *server) handleAudio(w http.ResponseWriter, r *http.Request, u store.User) {
	ownerID := r.PathValue("owner")
	slug, ok := strings.CutSuffix(r.PathValue("file"), ".mp3")
	if !ok || !store.ValidID(slug) {
		http.NotFound(w, r)
		return
	}
	if u.ID != ownerID {
		if _, err := s.store.GetShare(r.Context(), u.ID, ownerID, slug); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.serveAudio(w, r, ownerID, slug)
}

// serveAudio streams one enclosure. Callers do their own authorisation
// first; by the time it runs, the Episode has been established as
// visible to whoever is asking.
func (s *server) serveAudio(w http.ResponseWriter, r *http.Request, ownerID, slug string) {
	audio, err := s.store.OpenAudio(r.Context(), ownerID, slug)
	if err != nil {
		s.fail(w, err)
		return
	}
	if audio.RedirectURL != "" {
		http.Redirect(w, r, audio.RedirectURL, http.StatusFound)
		return
	}
	defer audio.Content.Close()
	w.Header().Set("Content-Type", audio.ContentType)
	http.ServeContent(w, r, slug+".mp3", audio.ModTime, audio.Content)
}

// handleCover serves Cover Art inside the feed's capability namespace,
// where podcast clients and any shared cache may keep it.
func (s *server) handleCover(w http.ResponseWriter, r *http.Request, u store.User) {
	// Cacheable: a replaced cover may take up to an hour to reach
	// clients (ADR 0003).
	s.cover(w, r, u, "public, max-age=3600")
}

// handleMyCover serves the same image to a signed-in browser. It is
// private: the URL carries no capability, so only this session's browser
// may keep a copy — never a shared proxy.
func (s *server) handleMyCover(w http.ResponseWriter, r *http.Request, u store.User) {
	s.cover(w, r, u, "private, max-age=3600")
}

func (s *server) cover(w http.ResponseWriter, r *http.Request, u store.User, cacheControl string) {
	cover, contentType, err := s.openCover(r, u, r.URL.Query().Get("s") == "thumb")
	if err != nil {
		s.fail(w, err)
		return
	}
	defer cover.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	io.Copy(w, cover)
}

// openCover returns the thumbnail when asked for it, falling back to the
// full-size image for covers uploaded before thumbnails existed. The
// full image is the RSS-facing default.
func (s *server) openCover(r *http.Request, u store.User, thumb bool) (io.ReadCloser, string, error) {
	if thumb {
		rc, ct, err := s.store.OpenCoverThumb(r.Context(), u.ID)
		if err == nil {
			return rc, ct, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, "", err
		}
	}
	return s.store.OpenCover(r.Context(), u.ID)
}

// handleQR renders the feed URL as a scannable QR code, so phone
// onboarding is a camera point instead of typing a token (ADR 0008).
func (s *server) handleQR(w http.ResponseWriter, r *http.Request, u store.User) {
	png, err := qrcode.Encode(s.feedURL(r, u), qrcode.Medium, 512)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(png)
}

// --- pages ---

// handleHome is the public landing page. A caller who already has a
// session is offered a trip to their dashboard on a short timer rather
// than being redirected outright: "/" is also how someone reaches the
// front door deliberately (to read it, to link it, to sign in as someone
// else), and a hard redirect would make that impossible from a logged-in
// browser. The timer is a nudge with an escape hatch, not a wall.
func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	var data struct {
		LoggedIn bool
		Title    string
	}
	if u, ok := s.sessionUser(r); ok {
		data.LoggedIn = true
		data.Title = u.Title
	}
	s.render(w, r, http.StatusOK, s.tmplHome, data)
}

// subscribeBox is the shared template data for every place the feed URL
// is offered: copy text, QR image, and the AntennaPod deep link.
type subscribeBox struct {
	FeedURL  string
	QRURL    string
	DeepLink string
}

func (s *server) subscribeBox(r *http.Request, u store.User) subscribeBox {
	feedURL := s.feedURL(r, u)
	return subscribeBox{
		FeedURL:  feedURL,
		QRURL:    "/f/" + u.FeedToken + "/qr.png",
		DeepLink: deepLink(feedURL),
	}
}

// handleFeedLanding is the subscribe page inside the capability
// namespace: the feed's identity plus every way to subscribe. Whoever
// holds the token can reach it — that is the point (ADR 0008).
func (s *server) handleFeedLanding(w http.ResponseWriter, r *http.Request, u store.User) {
	data := struct {
		User     store.User
		CoverURL string
		subscribeBox
	}{
		User:         u,
		subscribeBox: s.subscribeBox(r, u),
	}
	if u.CoverType != "" {
		data.CoverURL = "/f/" + u.FeedToken + "/cover"
	}
	s.render(w, r, http.StatusOK, s.tmplUser, data)
}

func (s *server) renderNotFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, s.tmplNotFound, nil)
}

// render buffers first so a template error can still become a 500. The
// request is what the navigation bar is built from (see nav.go); the
// page's own data goes underneath it untouched, so content templates
// keep the dot they always had.
func (s *server) render(w http.ResponseWriter, r *http.Request, status int, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", pageView{Nav: s.navFor(r), Page: data}); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func cacheControl(value string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", value)
		next.ServeHTTP(w, r)
	})
}

// --- Management API (/me) ---

// ensureFeedToken migrates users provisioned before ADR 0008: their
// first Dashboard or /me visit mints the Feed Token they never had.
func (s *server) ensureFeedToken(r *http.Request, u store.User) (store.User, error) {
	if u.FeedToken != "" {
		return u, nil
	}
	token, err := randomHex(16)
	if err != nil {
		return u, err
	}
	u.FeedToken = token
	return u, s.store.UpsertUser(r.Context(), u)
}

// handleGetMe answers browsers with the Dashboard page and everything
// else with JSON. The browser's Basic-auth prompt (username + publish
// token) is the login.
func (s *server) handleGetMe(w http.ResponseWriter, r *http.Request, u store.User) {
	u, err := s.ensureFeedToken(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		s.writeJSON(w, http.StatusOK, struct {
			store.User
			FeedURL string `json:"feed_url"`
		}{User: u, FeedURL: s.feedURL(r, u)})
		return
	}
	// The Dashboard shows the Personal Feed, not just what this user
	// published: own Episodes and shared-in ones in one log, which is
	// what the RSS feed has always carried. filter is the Feed Variant
	// parameter (ADR 0005), spelled the same here as on /me/feed.
	filter := r.URL.Query().Get("filter")
	if filter != "mine" && filter != "shared" && filter != "followed" {
		filter = ""
	}
	entries, err := s.feedEntries(r, u, "", filter)
	if err != nil {
		s.fail(w, err)
		return
	}
	// One query covers the whole Dashboard: every live link to anything
	// this user owns, grouped by Episode below (ADR 0014). Keyed by
	// owner/slug and never the bare slug — a shared Episode may carry
	// the same slug as one of this user's own, and it must not inherit
	// that Episode's links or the Revoke button that comes with them.
	invites, err := s.store.ListEpisodeInvites(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	now := time.Now()
	links := map[string][]episodeLink{}
	for _, inv := range invites {
		if !inv.Live(now) {
			continue
		}
		key := inv.OwnerID + "/" + inv.Slug
		links[key] = append(links[key], episodeLink{
			Token:    inv.Token,
			Minter:   inv.InviterID,
			Expires:  daysLeft(inv.ExpiresAt),
			Redeemed: inv.RedeemedBy != "",
		})
	}
	views := make([]episodeView, 0, len(entries))
	shared := 0
	for _, e := range entries {
		v := episodeView{
			Episode:   e.Episode,
			Published: relativeDate(e.PublishedAt),
			Duration:  humanDuration(e.DurationSec),
			PageURL:   episodeBase(u, e.Episode, true),
			// The cover art stays this feed's, for the reason
			// episodePage gives: a shared Episode is presented under
			// the art of the feed it arrived in. The credit line
			// carries the attribution the art cannot.
			Player: playerFor(u, e.Episode, true),
			Rank:   e.PublishedAt,
		}
		switch {
		case e.Delivered():
			// A Follow put this here. It is neither mine nor shared to
			// me, and the difference is not cosmetic: its audio lives
			// on the public Strand, because /me/episodes/{owner}/{slug}
			// authorises own-or-shared and would refuse it.
			v.Followed = true
			v.FromStrand = e.Strand
			v.Author = e.Author
			v.PageURL = "/strands/" + e.Strand
			v.Player.AudioURL = feed.StrandEnclosure("", e.Strand, e.AiringID)
			v.Player.CoverURL = "/strands/" + e.Strand + "/cover"
		case e.SharedAt == nil:
			v.Links = links[e.OwnerID+"/"+e.Slug]
			v.NeedsCharacters = s.generator != nil && e.Template == "stories" && len(e.Characters) == 0
		default:
			shared++
			v.Shared = true
			v.SharerID = e.SharerID
			v.Arrived = relativeDate(*e.SharedAt)
			if e.SharedAt.After(v.Rank) {
				v.Rank = *e.SharedAt
			}
		}
		views = append(views, v)
	}
	// Newest-to-me first. An Episode aired in April but shared with me
	// this morning is news; ranking it by its air date would bury it
	// where I would never see it arrive. The RSS feed keeps publication
	// order, which is where podcast clients expect it.
	sort.SliceStable(views, func(i, j int) bool {
		return views[i].Rank.After(views[j].Rank)
	})
	generations, err := s.dashboardGenerations(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The Beats card is a summary, not the page: enough to see that
	// something is running and to go look at it.
	var beats []beatView
	if s.generator != nil {
		beats, err = s.beatViews(r, u)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	// What is already on the air, and where anything else could go
	// (ADR 0018). One pair of reads for the whole page rather than two
	// per row.
	airStrands, err := s.awakeStrands(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	onAir, err := s.airingsBySlug(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	for i, v := range views {
		// Only the Owner's own rows: a Sharer may forward an Episode
		// onward but never put it in front of strangers (ADR 0006 vs
		// ADR 0018).
		if v.Shared {
			continue
		}
		if a, ok := onAir[v.Slug]; ok {
			airing := a
			views[i].OnAir = &airing
		}
		views[i].SuggestedStrand = v.Strand
	}
	s.render(w, r, http.StatusOK, s.tmplDashboard, struct {
		User            store.User
		FeedPage        string
		CoverURL        string
		Episodes        []episodeView
		SharedCount     int
		Filter          string
		GenerateEnabled bool
		Generations     []generationView
		Beats           []beatView
		// AirStrands is the canon a row may be aired into: awake
		// strands only, since a dormant or retired one takes nothing.
		// Empty means the airing controls do not appear at all.
		AirStrands []store.Strand
		// ReturnTo is this page as the reader reached it, filter and
		// all, so an action lands them back where they were rather than
		// at the top of an unfiltered log (ADR 0022).
		ReturnTo string
		subscribeBox
	}{
		User:            u,
		ReturnTo:        r.URL.RequestURI(),
		FeedPage:        "/f/" + u.FeedToken,
		CoverURL:        sessionCoverURL(u),
		Episodes:        views,
		SharedCount:     shared,
		Filter:          filter,
		GenerateEnabled: s.generator != nil,
		Generations:     generations,
		Beats:           beats,
		AirStrands:      airStrands,
		subscribeBox:    s.subscribeBox(r, u),
	})
	// Opening the Dashboard catches your Beats up, so a feed you are
	// looking at is never quietly overdue.
	s.heartbeat(u)
}

// episodeView is an Episode plus the display strings the Dashboard
// shows for it, precomputed like inviteView/generationView.
type episodeView struct {
	store.Episode
	// Published is when the Episode was made, in relative words. Not
	// "Aired": since ADR 0018 that word means going public, and this
	// field predates it by months.
	Published string
	Duration  string
	// PageURL is this Episode's own page, inside the Feed Token
	// namespace — the Dashboard title links to it.
	PageURL string
	Player  playerView
	// Links are the live Invites carrying this Episode — every way it
	// can currently be heard outside the membership (ADR 0014).
	Links []episodeLink
	// NeedsCharacters offers the "save characters" backfill button: a
	// story episode whose cast was never extracted.
	NeedsCharacters bool

	// OnAir is this Episode's live Airing, or nil when it is private.
	// Only ever set on the Owner's own rows: a Sharer may forward an
	// Episode but never air it (ADR 0018).
	OnAir *store.Airing
	// SuggestedStrand is where the strand picker starts — what the
	// station chose at generation time, when it chose anything. The
	// station proposes, the Owner disposes (ADR 0017).
	SuggestedStrand string

	// Shared marks an Episode that reached this feed as a Share rather
	// than being published into it. It decides both the credit line and
	// which controls the row offers: a Share may be forwarded on or
	// dropped from the feed, but never deleted, revoked, or edited —
	// none of it is this user's to change (ADR 0006).
	Shared bool
	// SharerID is whoever placed the Episode in this feed, which may
	// differ from the Owner since any recipient may forward. Equal to
	// OwnerID when the creator shared it directly, and the credit line
	// says so once rather than twice.
	SharerID string
	// Arrived is when the Share landed, in the same relative words as
	// Published. Empty on own Episodes, which arrived by being made.
	Arrived string

	// Followed marks an Episode a Follow delivered (ADR 0019). It is
	// the third kind of row, and the reason this struct no longer lets
	// "not Shared" stand for "mine": a followed Episode is nobody's
	// here to air, delete, or send as a link.
	Followed bool
	// FromStrand names where a followed Episode came from, for the
	// credit line and the link back to it. Deliberately not "Strand":
	// that name belongs to the embedded Episode, where it means the
	// subject the station sorted it into, and shadowing it silently
	// emptied the air picker's default.
	FromStrand string
	// Author is the Owner's feed title on a followed row — the same
	// attribution the Strand Page shows, never the username.
	Author string
	// Rank orders the log: when the Episode became news to this user,
	// which for a Share is its arrival and not its air date.
	Rank time.Time
}

// episodeLink is one live Invite to an Episode, as its Owner sees it:
// who minted it, when it dies, and whether it has already admitted
// someone. The Owner may revoke any of them.
type episodeLink struct {
	Token    string
	Minter   string
	Expires  string
	Redeemed bool
}

// daysLeft says how long something has, in whole days, for a UI where
// "12 days left" is the useful precision and a timestamp is not.
func daysLeft(until time.Time) string {
	d := time.Until(until)
	switch days := int(d.Hours() / 24); {
	case d <= 0:
		return "expired"
	case days == 0:
		return "today"
	case days == 1:
		return "1 day left"
	default:
		return fmt.Sprintf("%d days left", days)
	}
}

// playerView is everything the inline Player needs, and nothing else:
// the enclosure to play, a duration known before a byte is fetched (so
// the scrubber does not resize on loadedmetadata), and the labels Media
// Session shows on a lock screen. Key names this Episode's Resume
// Position in browser storage — it never reaches the server (ADR 0013).
type playerView struct {
	AudioURL string
	Title    string
	Seconds  int
	Key      string
	CoverURL string
}

// episodeBase is where one Episode's two representations live. A signed-in
// browser is addressed under /me, authorised by its session; everyone else
// gets the capability namespace, where the URL is the credential.
//
// Which one matters for privacy, not just tidiness: a URL under /f/ IS the
// whole feed, so anything a browser might copy out of its address bar —
// or hand to a site in a Referer header — is best kept free of it (ADR
// 0008, 0013).
func episodeBase(u store.User, ep store.Episode, session bool) string {
	if session {
		return "/me/episodes/" + ep.OwnerID + "/" + ep.Slug
	}
	return "/f/" + u.FeedToken + "/" + ep.OwnerID + "/" + ep.Slug
}

// audioURL is the enclosure address; under /f/ it matches the URL the RSS
// feed hands podcast clients.
func audioURL(u store.User, ep store.Episode, session bool) string {
	return episodeBase(u, ep, session) + ".mp3"
}

func playerFor(u store.User, ep store.Episode, session bool) playerView {
	cover := coverURL(u)
	if session {
		cover = sessionCoverURL(u)
	}
	return playerView{
		AudioURL: audioURL(u, ep, session),
		Title:    ep.Title,
		Seconds:  ep.DurationSec,
		Key:      ep.OwnerID + "/" + ep.Slug,
		CoverURL: cover,
	}
}

// coverURL is where the owner's Cover Art is served, or "" without one.
func coverURL(u store.User) string {
	if u.CoverType == "" {
		return ""
	}
	return "/f/" + u.FeedToken + "/cover"
}

// sessionCoverURL is the same image for a signed-in page, which has no
// business carrying a capability it does not need.
func sessionCoverURL(u store.User) string {
	if u.CoverType == "" {
		return ""
	}
	return "/me/image"
}

// relativeDate renders a publish time the way a program log would:
// close dates relative, older ones absolute.
func relativeDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 24*time.Hour:
		return "today"
	case d < 48*time.Hour:
		return "yesterday"
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dw ago", int(d.Hours()/(24*7)))
	default:
		return t.Format("Jan 2, 2006")
	}
}

func humanDuration(sec int) string {
	switch {
	case sec <= 0:
		return ""
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	default:
		return fmt.Sprintf("%d min", (sec+30)/60)
	}
}

// handleGetSettings renders the Settings page: everything about the
// account that isn't the daily publish/share loop. Session-only, like
// the Credential Management API it fronts.
func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request, u store.User) {
	u, err := s.ensureFeedToken(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	invs, err := s.store.ListInvites(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	pending := []inviteView{}
	for _, inv := range invs {
		if v := s.inviteView(r, inv); v.Status == "pending" {
			pending = append(pending, v)
		}
	}
	apiKeys, err := s.store.ListAPIKeys(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, s.tmplSettings, struct {
		User          store.User
		CoverURL      string
		Invites       []inviteView
		APIKeys       []store.APIKey
		HasPassword   bool
		GoogleLinked  bool
		GoogleEmail   string
		GoogleEnabled bool
		Version       string
		subscribeBox
	}{
		User:          u,
		CoverURL:      coverURL(u),
		Invites:       pending,
		APIKeys:       apiKeys,
		HasPassword:   u.PasswordHash != "",
		GoogleLinked:  u.GoogleSub != "",
		GoogleEmail:   u.GoogleEmail,
		GoogleEnabled: s.google != nil,
		Version:       s.version,
		subscribeBox:  s.subscribeBox(r, u),
	})
}

// dashboardGenerations lists the caller's Generations still worth a row:
// in flight or failed (done ones are already visible as episodes).
func (s *server) dashboardGenerations(r *http.Request, u store.User) ([]generationView, error) {
	if s.generator == nil {
		return nil, nil
	}
	gens, err := s.store.ListGenerations(r.Context(), u.ID)
	if err != nil {
		return nil, err
	}
	views := []generationView{}
	for _, g := range gens {
		if g.Stage == store.GenDone {
			continue
		}
		views = append(views, s.generationView(g))
		if len(views) == 5 {
			break
		}
	}
	return views, nil
}

// handleSearchUsers is the member directory behind the Dashboard's
// share box: authenticated members may find each other by name; the
// Public Surface still exposes nothing.
func (s *server) handleSearchUsers(w http.ResponseWriter, r *http.Request, u store.User) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	type hit struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	hits := []hit{}
	for _, v := range users {
		if v.ID == u.ID {
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(v.ID), q) || strings.Contains(strings.ToLower(v.Title), q) {
			hits = append(hits, hit{ID: v.ID, Title: v.Title})
			if len(hits) == 20 {
				break
			}
		}
	}
	s.writeJSON(w, http.StatusOK, hits)
}

func (s *server) handleUpdateMe(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Language    string `json:"language"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	u.Title, u.Description, u.Language = req.Title, req.Description, req.Language
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	u, _ = s.store.GetUser(r.Context(), u.ID)
	s.writeJSON(w, http.StatusOK, u)
}

// handleResetFeedToken is the self-service leak response: mint a new
// Feed Token, killing the old URL instantly. Costs a resubscribe; risks
// nothing but read access (ADR 0008).
func (s *server) handleResetFeedToken(w http.ResponseWriter, r *http.Request, u store.User) {
	token, err := randomHex(16)
	if err != nil {
		s.fail(w, err)
		return
	}
	u.FeedToken = token
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"feed_url": s.feedURL(r, u)})
}

func (s *server) handleSetCover(w http.ResponseWriter, r *http.Request, u store.User) {
	contentType := r.Header.Get("Content-Type")
	if contentType != "image/jpeg" && contentType != "image/png" {
		http.Error(w, "Content-Type must be image/jpeg or image/png", http.StatusUnsupportedMediaType)
		return
	}
	body := http.MaxBytesReader(w, r.Body, 8<<20)
	p, err := coverart.Process(body, contentType)
	if err != nil {
		http.Error(w, "could not process image: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.SetCover(r.Context(), u.ID, p.FullType, bytes.NewReader(p.Full), bytes.NewReader(p.Thumb)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleListFeed(w http.ResponseWriter, r *http.Request, u store.User) {
	entries, err := s.feedEntries(r, u, r.URL.Query().Get("from"), r.URL.Query().Get("filter"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, entries)
}

func (s *server) handleListEpisodes(w http.ResponseWriter, r *http.Request, u store.User) {
	episodes, err := s.store.ListEpisodes(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, episodes)
}

// handlePublish is the Publishing Contract: multipart/form-data with a
// "metadata" JSON field and an "audio" file field, into the caller's own
// feed. Publishing an existing slug replaces the episode (ADR 0002).
func (s *server) handlePublish(w http.ResponseWriter, r *http.Request, u store.User) {
	slug := r.PathValue("slug")
	if !store.ValidID(slug) {
		http.Error(w, "invalid slug (want ^[a-z0-9][a-z0-9._-]*$)", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		http.Error(w, "bad multipart body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var meta struct {
		Title           string    `json:"title"`
		Description     string    `json:"description"`
		PublishedAt     time.Time `json:"published_at"`
		DurationSeconds int       `json:"duration_seconds"`
	}
	rawMeta := r.FormValue("metadata")
	if rawMeta == "" {
		http.Error(w, `missing "metadata" field`, http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal([]byte(rawMeta), &meta); err != nil {
		http.Error(w, "bad metadata JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if meta.Title == "" {
		http.Error(w, "metadata.title is required", http.StatusBadRequest)
		return
	}
	if meta.PublishedAt.IsZero() {
		meta.PublishedAt = time.Now().UTC()
	}

	audioFile, _, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, `missing "audio" file field`, http.StatusBadRequest)
		return
	}
	defer audioFile.Close()

	// A dumb publisher may omit the duration; estimate it from the MP3
	// frames. An explicit duration_seconds always wins (ADR 0004).
	if meta.DurationSeconds == 0 {
		if d, err := audio.MP3Duration(audioFile); err == nil {
			meta.DurationSeconds = int(d.Round(time.Second).Seconds())
		} else {
			s.log.Warn("could not estimate duration", "owner", u.ID, "slug", slug, "err", err)
		}
		if _, err := audioFile.Seek(0, io.SeekStart); err != nil {
			s.fail(w, err)
			return
		}
	}

	_, err = s.store.GetEpisode(r.Context(), u.ID, slug)
	replaced := err == nil
	ep, err := s.store.UpsertEpisode(r.Context(), store.Episode{
		OwnerID:     u.ID,
		Slug:        slug,
		Title:       meta.Title,
		Description: meta.Description,
		PublishedAt: meta.PublishedAt.UTC(),
		DurationSec: meta.DurationSeconds,
		AudioType:   "audio/mpeg",
	}, audioFile)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusCreated
	if replaced {
		status = http.StatusOK
	}
	s.writeJSON(w, status, ep)
}

func (s *server) handleDeleteEpisode(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := s.store.DeleteEpisode(r.Context(), u.ID, r.PathValue("slug")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleShare places an episode from the caller's feed into one or more
// other users' feeds. Anyone may share what is in their feed, own or
// shared — forwarding is allowed (ADR 0006). The "to" field carries one
// username or several at once ("nico, ldipenti"); a single recipient
// keeps the plain HTTP-status contract, several recipients get a per-name
// JSON summary since no one status can describe a mixed result.
func (s *server) handleShare(w http.ResponseWriter, r *http.Request, u store.User) {
	ownerID, slug := r.PathValue("owner"), r.PathValue("slug")
	var req struct {
		To string `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	recipients := parseRecipients(req.To)
	if len(recipients) == 0 {
		http.Error(w, "no recipients", http.StatusBadRequest)
		return
	}

	// The episode must be in the caller's feed: their own, or shared in.
	// It is the same episode for every recipient, so check it once.
	if err := s.inFeed(r, u, ownerID, slug); err != nil {
		s.fail(w, err)
		return
	}

	// One recipient: answer with the original status codes so a caller
	// (and the tests) can read the outcome straight off the HTTP status.
	if len(recipients) == 1 {
		out, err := s.shareOne(r, u, ownerID, slug, recipients[0])
		if err != nil {
			s.fail(w, err)
			return
		}
		if out.code >= 400 {
			http.Error(w, out.reason, out.code)
			return
		}
		w.WriteHeader(out.code)
		return
	}

	// Several recipients: one bad name must not sink the batch, so collect
	// every outcome and report it. "shared" holds names now in their feed
	// (freshly placed or already there); "failed" maps the rest to a reason.
	res := struct {
		Shared []string          `json:"shared"`
		Failed map[string]string `json:"failed,omitempty"`
	}{Failed: map[string]string{}}
	for _, to := range recipients {
		out, err := s.shareOne(r, u, ownerID, slug, to)
		if err != nil {
			s.log.Error("share failed", "to", to, "owner", ownerID, "slug", slug, "err", err)
			res.Failed[to] = "could not share"
			continue
		}
		if out.code >= 400 {
			res.Failed[to] = out.reason
		} else {
			res.Shared = append(res.Shared, to)
		}
	}
	if len(res.Failed) == 0 {
		res.Failed = nil
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		s.log.Error("encode share result", "err", err)
	}
}

// parseRecipients splits a share target into distinct usernames. The box
// takes several at once, separated however feels natural — "nico,
// ldipenti", "nico ldipenti", newlines — so any run of commas and
// whitespace is a separator. Order is kept and duplicates dropped.
func parseRecipients(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	}) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// shareOutcome is what happened placing one episode into one recipient's
// feed: an HTTP status matching the single-recipient contract (201 freshly
// shared, 204 already there / their own, 4xx refused) and a short human
// reason for the outcomes that added nothing.
type shareOutcome struct {
	code   int
	reason string
}

// shareOne places the episode (ownerID/slug) into recipient `to`'s feed on
// behalf of sharer u. The episode is assumed already known to be in u's
// feed. A returned error is an infrastructure failure; otherwise the
// outcome's code carries the per-recipient result.
func (s *server) shareOne(r *http.Request, u store.User, ownerID, slug, to string) (shareOutcome, error) {
	if to == u.ID {
		return shareOutcome{http.StatusBadRequest, "that's you"}, nil
	}
	recipient, err := s.store.GetUser(r.Context(), to)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return shareOutcome{http.StatusNotFound, "no such user"}, nil
		}
		return shareOutcome{}, err
	}
	if recipient.Blocked(u.ID) {
		return shareOutcome{http.StatusForbidden, "has blocked you"}, nil
	}
	if recipient.ID == ownerID {
		return shareOutcome{http.StatusNoContent, "it is their own episode"}, nil
	}
	if _, err := s.store.GetShare(r.Context(), recipient.ID, ownerID, slug); err == nil {
		return shareOutcome{http.StatusNoContent, "already in their feed"}, nil
	}
	err = s.store.AddShare(r.Context(), store.Share{
		UserID:   recipient.ID,
		OwnerID:  ownerID,
		Slug:     slug,
		SharerID: u.ID,
		SharedAt: time.Now().UTC(),
	})
	if err != nil {
		return shareOutcome{}, err
	}
	return shareOutcome{http.StatusCreated, ""}, nil
}

// handleRemoveShare takes a shared episode out of the caller's own feed.
// The caller's own episodes are deleted via DELETE /me/episodes/{slug}.
func (s *server) handleRemoveShare(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := s.store.RemoveShare(r.Context(), u.ID, r.PathValue("owner"), r.PathValue("slug")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetList adds or removes an entry on the caller's block or mute
// list, depending on method and path (ADR 0006).
func (s *server) handleSetList(w http.ResponseWriter, r *http.Request, u store.User) {
	target := r.PathValue("user")
	if target == u.ID {
		http.Error(w, "cannot block or mute yourself", http.StatusBadRequest)
		return
	}
	list := &u.Blocks
	if strings.HasPrefix(r.URL.Path, "/me/mutes/") {
		list = &u.Mutes
	}
	switch r.Method {
	case http.MethodPut:
		if _, err := s.store.GetUser(r.Context(), target); err != nil {
			s.fail(w, err)
			return
		}
		if !slices.Contains(*list, target) {
			*list = append(*list, target)
			sort.Strings(*list)
		}
	case http.MethodDelete:
		*list = slices.DeleteFunc(*list, func(v string) bool { return v == target })
	}
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- admin ---

func (s *server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, users)
}

// handleCreateUser provisions a user with a temporary password, shown
// exactly once in the response; only the hash is stored (ADR 0005).
func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("user")
	if err := store.ValidateUsername(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.store.GetUser(r.Context(), id); err == nil {
		http.Error(w, "user exists", http.StatusConflict)
		return
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Language    string `json:"language"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Title == "" {
		req.Title = id
	}

	password, hash, err := tempPassword()
	if err != nil {
		s.fail(w, err)
		return
	}
	feedToken, err := randomHex(16)
	if err != nil {
		s.fail(w, err)
		return
	}
	u := store.User{
		ID:           id,
		Title:        req.Title,
		Description:  req.Description,
		Language:     req.Language,
		FeedToken:    feedToken,
		PasswordHash: hash,
	}
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]string{
		"id":       id,
		"password": password,
		"feed_url": s.feedURL(r, u),
	})
}

// handlePasswordReset is the recovery path: there is no self-service
// reset (no email exists in this system), so a locked-out user asks the
// operator for a temporary password (ADR 0007/0010). Every session dies;
// API keys and the Feed Token are untouched.
func (s *server) handlePasswordReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("user")
	u, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	password, hash, err := tempPassword()
	if err != nil {
		s.fail(w, err)
		return
	}
	u.PasswordHash = hash
	u.CredentialVersion++
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{
		"id":       id,
		"password": password,
	})
}

// handleSetAdmin appoints an admin. This is the break-glass route and the
// only one still guarded by ADMIN_TOKEN: every other /admin endpoint
// requires a logged-in admin, and something has to be able to create the
// first one. It is also the recovery path if the last admin is locked
// out, which is why it stays on the token rather than becoming an
// admin-only action.
func (s *server) handleSetAdmin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("user")
	u, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	u.Admin = true
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "admin": true})
}

// tempPassword mints the operator-issued temporary password and its
// bcrypt hash; the user changes it from the dashboard.
func tempPassword() (password, hash string, err error) {
	password, err = randomHex(12)
	if err != nil {
		return "", "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", "", err
	}
	return password, string(h), nil
}

func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteUser(r.Context(), r.PathValue("user")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- invites (ADR 0007) ---

// inFeed reports whether the episode is in u's Personal Feed — their own
// or shared in — which is the license to share or invite with it.
func (s *server) inFeed(r *http.Request, u store.User, ownerID, slug string) error {
	if u.ID != ownerID {
		if _, err := s.store.GetShare(r.Context(), u.ID, ownerID, slug); err != nil {
			return err
		}
	}
	_, err := s.store.GetEpisode(r.Context(), ownerID, slug)
	return err
}

// inviteView is an Invite as the inviter sees it: with its URL and a
// computed status.
type inviteView struct {
	store.Invite
	URL    string `json:"url"`
	Status string `json:"status"` // pending | redeemed | expired
}

func (s *server) inviteView(r *http.Request, inv store.Invite) inviteView {
	v := inviteView{Invite: inv, URL: s.base(r) + "/invites/" + inv.Token, Status: "pending"}
	switch {
	case inv.RedeemedBy != "":
		v.Status = "redeemed"
	case !inv.Redeemable(time.Now()):
		v.Status = "expired"
	}
	return v
}

func (s *server) handleCreateInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Owner string `json:"owner"`
		Slug  string `json:"slug"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if (req.Owner == "") != (req.Slug == "") {
		http.Error(w, "payload needs both owner and slug", http.StatusBadRequest)
		return
	}
	if req.Owner != "" {
		if err := s.inFeed(r, u, req.Owner, req.Slug); err != nil {
			s.fail(w, err)
			return
		}
	}
	token, err := randomHex(16)
	if err != nil {
		s.fail(w, err)
		return
	}
	now := time.Now().UTC()
	inv := store.Invite{
		Token:     token,
		InviterID: u.ID,
		OwnerID:   req.Owner,
		Slug:      req.Slug,
		CreatedAt: now,
		ExpiresAt: now.Add(inviteTTL),
	}
	if err := s.store.AddInvite(r.Context(), inv); err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.inviteView(r, inv))
}

func (s *server) handleListInvites(w http.ResponseWriter, r *http.Request, u store.User) {
	invs, err := s.store.ListInvites(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	views := make([]inviteView, len(invs))
	for i, inv := range invs {
		views[i] = s.inviteView(r, inv)
	}
	s.writeJSON(w, http.StatusOK, views)
}

// handleRevokeInvite kills an Invite. Two people may: whoever minted it,
// and the Owner of the Episode it carries — the Owner's lever against a
// link they did not mint, short of deleting the Episode for everyone
// (ADR 0014).
func (s *server) handleRevokeInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	inv, err := s.store.GetInvite(r.Context(), r.PathValue("token"))
	if err != nil || (inv.InviterID != u.ID && inv.OwnerID != u.ID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// A spent Invite that still plays is exactly what an Owner needs to
	// be able to kill, so redemption no longer blocks revocation — it
	// only means the door is already closed.
	if inv.RedeemedBy != "" && inv.OwnerID != u.ID {
		http.Error(w, "already redeemed", http.StatusConflict)
		return
	}
	if err := s.store.DeleteInvite(r.Context(), inv.Token); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// invitePage is the template data for the invite page: the Episode a
// Guest may hear, and the Redemption form underneath it.
type invitePage struct {
	Inviter            string
	EpisodeTitle       string
	EpisodeDescription string
	CoverURL           string
	Player             playerView
	HasEpisode         bool
	// Redeemable is false once the Invite has been spent. The Episode
	// keeps playing for the rest of its term; only the door has closed.
	Redeemable    bool
	Username      string
	Error         string
	GoogleEnabled bool
}

// liveInvite loads an invite that still plays, or renders the styled 404
// — an invalid or expired token looks like any other missing page. A
// spent invite is still live: Redemption closes the door, not the sound
// (ADR 0014). Callers that admit users check Redeemable themselves.
func (s *server) liveInvite(w http.ResponseWriter, r *http.Request) (store.Invite, bool) {
	inv, err := s.store.GetInvite(r.Context(), r.PathValue("token"))
	if err != nil || !inv.Live(time.Now()) {
		s.renderNotFound(w, r)
		return store.Invite{}, false
	}
	return inv, true
}

// guestEpisode resolves the Episode an Invite carries, for someone with
// no account. A dead payload (the Owner deleted the Episode) reports
// missing and the page silently omits it, consistent with share
// semantics (ADR 0006) — an Owner's delete reaches Guests too.
func (s *server) guestEpisode(r *http.Request, inv store.Invite) (store.Episode, bool) {
	if inv.OwnerID == "" {
		return store.Episode{}, false
	}
	ep, err := s.store.GetEpisode(r.Context(), inv.OwnerID, inv.Slug)
	return ep, err == nil
}

func (s *server) invitePageData(r *http.Request, inv store.Invite) invitePage {
	data := invitePage{
		Inviter:       inv.InviterID,
		GoogleEnabled: s.google != nil,
		Redeemable:    inv.Redeemable(time.Now()),
	}
	ep, ok := s.guestEpisode(r, inv)
	if !ok {
		return data
	}
	// Everything a Guest gets is addressed inside this invite's own
	// namespace: one Episode, and no way to ask about any other (ADR
	// 0014). No download link — that would outlive the Owner's delete.
	base := "/invites/" + inv.Token
	data.HasEpisode = true
	data.EpisodeTitle = ep.Title
	data.EpisodeDescription = ep.Description
	data.Player = playerView{
		AudioURL: base + "/audio.mp3",
		Title:    ep.Title,
		Seconds:  ep.DurationSec,
		Key:      "invite/" + inv.Token,
	}
	if owner, err := s.store.GetUser(r.Context(), inv.OwnerID); err == nil && owner.CoverType != "" {
		data.CoverURL = base + "/cover"
		data.Player.CoverURL = data.CoverURL
	}
	return data
}

func (s *server) handleInvitePage(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.liveInvite(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, s.tmplInvite, s.invitePageData(r, inv))
}

// handleInviteAudio streams the one Episode an Invite carries. The token
// is the whole credential and unlocks nothing else.
func (s *server) handleInviteAudio(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.liveInvite(w, r)
	if !ok {
		return
	}
	ep, found := s.guestEpisode(r, inv)
	if !found {
		s.renderNotFound(w, r)
		return
	}
	s.serveAudio(w, r, ep.OwnerID, ep.Slug)
}

// handleInviteCover serves the Cover Art of the feed the Episode came
// from, so the Guest page has a face. Private: the URL is a capability.
func (s *server) handleInviteCover(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.liveInvite(w, r)
	if !ok {
		return
	}
	ep, found := s.guestEpisode(r, inv)
	if !found {
		s.renderNotFound(w, r)
		return
	}
	owner, err := s.store.GetUser(r.Context(), ep.OwnerID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.cover(w, r, owner, "private, max-age=3600")
}

// handleRedeem turns an Invite into a User. The invitee picks their
// username and their Login: setting a password finishes right here;
// "Join with Google" detours through the consent screen and finishes in
// finishGoogleRedemption (ADR 0007/0010).
func (s *server) handleRedeem(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.liveInvite(w, r)
	if !ok {
		return
	}
	// Live but spent: the page still plays, the door is closed. An
	// Invite admits exactly one User, however long it keeps playing.
	if !inv.Redeemable(time.Now()) {
		s.renderNotFound(w, r)
		return
	}
	retry := func(status int, msg, username string) {
		data := s.invitePageData(r, inv)
		data.Error, data.Username = msg, username
		s.render(w, r, status, s.tmplInvite, data)
	}

	username := r.FormValue("username")
	if err := store.ValidateUsername(username); err != nil {
		// The message names the rule broken, so it can be shown as-is.
		retry(http.StatusBadRequest, upperFirst(err.Error())+".", username)
		return
	}
	// Availability is checked before the invite is spent, so a taken
	// name never burns the invite.
	if _, err := s.store.GetUser(r.Context(), username); err == nil {
		retry(http.StatusConflict, "That username is taken — pick another.", username)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}

	if r.FormValue("method") == "google" {
		if s.google == nil {
			retry(http.StatusBadRequest, "Google sign-in is not available on this server — set a password instead.", username)
			return
		}
		s.startGoogle(w, r, oauthState{Mode: "redeem", Invite: inv.Token, User: username})
		return
	}

	password := r.FormValue("password")
	if len(password) < minPasswordLen {
		retry(http.StatusBadRequest, fmt.Sprintf("Password must be at least %d characters.", minPasswordLen), username)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.completeRedemption(w, r, inv, store.User{
		ID:           username,
		Title:        username,
		PasswordHash: string(hash),
	})
}

// completeRedemption is the shared tail of both Redemption paths: u
// arrives with its Login already set (password hash or Google identity),
// the invite is claimed, the Feed Token minted, the payload delivered,
// and the browser leaves logged in on the Welcome page.
func (s *server) completeRedemption(w http.ResponseWriter, r *http.Request, inv store.Invite, u store.User) {
	if err := s.store.RedeemInvite(r.Context(), inv.Token, u.ID); err != nil {
		// Lost a race with another redemption or a revocation.
		s.renderNotFound(w, r)
		return
	}
	feedToken, err := randomHex(16)
	if err != nil {
		s.fail(w, err)
		return
	}
	u.FeedToken = feedToken
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}

	sharedTitle := ""
	if inv.OwnerID != "" && inv.OwnerID != u.ID {
		if ep, err := s.store.GetEpisode(r.Context(), inv.OwnerID, inv.Slug); err == nil {
			sharedTitle = ep.Title
			if err := s.store.AddShare(r.Context(), store.Share{
				UserID:   u.ID,
				OwnerID:  inv.OwnerID,
				Slug:     inv.Slug,
				SharerID: inv.InviterID,
				SharedAt: time.Now().UTC(),
			}); err != nil {
				s.log.Warn("invite payload share failed", "invite", inv.Token, "err", err)
				sharedTitle = ""
			}
		}
	}

	s.setSession(w, r, u)
	s.render(w, r, http.StatusOK, s.tmplWelcome, struct {
		Username    string
		SharedTitle string
		subscribeBox
	}{
		Username:     u.ID,
		SharedTitle:  sharedTitle,
		subscribeBox: s.subscribeBox(r, u),
	})
}

// --- helpers ---

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.log.Error("internal error", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
