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

	qrcode "github.com/skip2/go-qrcode"

	"github.com/nicocesar/podcasting_server/internal/audio"
	"github.com/nicocesar/podcasting_server/internal/feed"
	"github.com/nicocesar/podcasting_server/internal/generation"
	"github.com/nicocesar/podcasting_server/internal/store"
)

// maxUploadBytes caps write-request bodies. Cloud Run itself caps HTTP/1
// requests at 32 MiB; this is a backstop for local development.
const maxUploadBytes = 256 << 20

// inviteTTL bounds how long an unredeemed Invite stays a live door into
// the system (ADR 0007).
const inviteTTL = 7 * 24 * time.Hour

type Config struct {
	Store store.Store
	// BaseURL overrides the external base URL used in feed links. When
	// empty, it is derived per-request from Host and X-Forwarded-Proto,
	// which is correct behind Cloud Run.
	BaseURL string
	// AdminToken guards the /admin endpoints (Authorization: Bearer).
	// Users authenticate with their own credentials (ADR 0005).
	AdminToken string
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
}

type server struct {
	store     store.Store
	baseURL   string
	adminHash [32]byte
	log       *slog.Logger
	generator *generation.Runner
	adminAPI  *anthropicAdmin

	tmplHome       *template.Template
	tmplUser       *template.Template
	tmplInvite     *template.Template
	tmplWelcome    *template.Template
	tmplDashboard  *template.Template
	tmplNotFound   *template.Template
	tmplGenerate   *template.Template
	tmplGeneration *template.Template
}

func New(cfg Config) (http.Handler, error) {
	if cfg.AdminToken == "" {
		return nil, errors.New("httpapi: AdminToken must be set")
	}
	s := &server{
		store:     cfg.Store,
		baseURL:   strings.TrimSuffix(cfg.BaseURL, "/"),
		adminHash: sha256.Sum256([]byte(cfg.AdminToken)),
		log:       cfg.Logger,
		generator: cfg.Generator,
		adminAPI:  newAnthropicAdmin(cfg.AnthropicAdminKey, cfg.AnthropicAdminBaseURL),
	}
	if s.log == nil {
		s.log = slog.Default()
	}

	// Each page is layout + its content template (+ shared fragments).
	for _, p := range []struct {
		dst   **template.Template
		files []string
	}{
		{&s.tmplHome, []string{"templates/layout.html", "templates/home.html"}},
		{&s.tmplUser, []string{"templates/layout.html", "templates/user.html", "templates/fragments/*.html"}},
		{&s.tmplInvite, []string{"templates/layout.html", "templates/invite.html"}},
		{&s.tmplWelcome, []string{"templates/layout.html", "templates/welcome.html", "templates/fragments/*.html"}},
		{&s.tmplDashboard, []string{"templates/layout.html", "templates/dashboard.html"}},
		{&s.tmplNotFound, []string{"templates/layout.html", "templates/notfound.html"}},
		{&s.tmplGenerate, []string{"templates/layout.html", "templates/generate.html"}},
		{&s.tmplGeneration, []string{"templates/layout.html", "templates/generation.html"}},
	} {
		t, err := template.ParseFS(cfg.Assets, p.files...)
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
	mux.HandleFunc("GET /invites/{token}", s.handleInvitePage)
	mux.HandleFunc("POST /invites/{token}", s.handleRedeem)
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheControl("public, max-age=86400", http.FileServerFS(static))))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		s.renderNotFound(w)
	})

	// Read side (ADR 0008): the Feed Token capability namespace. The
	// URL is the credential — podcast clients never see an auth dialog.
	mux.HandleFunc("GET /f/{token}", s.feed(s.handleFeedLanding))
	mux.HandleFunc("GET /f/{token}/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/f/"+r.PathValue("token"), http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /f/{token}/feed.xml", s.feed(s.handleFeed))
	mux.HandleFunc("GET /f/{token}/cover", s.feed(s.handleCover))
	mux.HandleFunc("GET /f/{token}/qr.png", s.feed(s.handleQR))
	mux.HandleFunc("GET /f/{token}/{owner}/{file}", s.feed(s.handleAudio))

	// Publishing Contract + Management API (the user's publish token).
	// Everything is scoped to the caller: publishing into someone else's
	// feed is inexpressible (ADR 0005).
	mux.HandleFunc("GET /me", s.auth(s.handleGetMe))
	mux.HandleFunc("GET /me/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/me", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /me/users", s.auth(s.handleSearchUsers))
	mux.HandleFunc("PUT /me", s.auth(s.handleUpdateMe))
	mux.HandleFunc("PUT /me/image", s.auth(s.handleSetCover))
	mux.HandleFunc("POST /me/feed-token", s.auth(s.handleResetFeedToken))
	mux.HandleFunc("GET /me/feed", s.auth(s.handleListFeed))
	mux.HandleFunc("GET /me/episodes", s.auth(s.handleListEpisodes))
	mux.HandleFunc("PUT /me/episodes/{slug}", s.auth(s.handlePublish))
	mux.HandleFunc("DELETE /me/episodes/{slug}", s.auth(s.handleDeleteEpisode))
	mux.HandleFunc("POST /me/feed/{owner}/{slug}/share", s.auth(s.handleShare))
	mux.HandleFunc("DELETE /me/feed/{owner}/{slug}", s.auth(s.handleRemoveShare))
	mux.HandleFunc("PUT /me/blocks/{user}", s.auth(s.handleSetList))
	mux.HandleFunc("DELETE /me/blocks/{user}", s.auth(s.handleSetList))
	mux.HandleFunc("PUT /me/mutes/{user}", s.auth(s.handleSetList))
	mux.HandleFunc("DELETE /me/mutes/{user}", s.auth(s.handleSetList))
	mux.HandleFunc("POST /me/invites", s.auth(s.handleCreateInvite))
	mux.HandleFunc("GET /me/invites", s.auth(s.handleListInvites))
	mux.HandleFunc("DELETE /me/invites/{token}", s.auth(s.handleRevokeInvite))

	// Built-in Generation (ADR 0009): topic in, Episode in the caller's
	// own feed out, with an observable in-between.
	mux.HandleFunc("GET /me/generate", s.auth(s.generating(s.handleGeneratePage)))
	mux.HandleFunc("POST /me/generate", s.auth(s.generating(s.handleGenerateStart)))
	mux.HandleFunc("GET /me/generations/{id}", s.auth(s.generating(s.handleGeneration)))
	mux.HandleFunc("POST /me/generations/{id}/retry", s.auth(s.generating(s.handleGenerationRetry)))

	// Admin: fallback provisioning and credential recovery (ADR 0007).
	mux.HandleFunc("GET /admin/users", s.admin(s.handleListUsers))
	mux.HandleFunc("PUT /admin/users/{user}", s.admin(s.handleCreateUser))
	mux.HandleFunc("DELETE /admin/users/{user}", s.admin(s.handleDeleteUser))
	mux.HandleFunc("POST /admin/users/{user}/credentials", s.admin(s.handleRotateCredentials))

	// Admin cost reporting: real billed dollars from Anthropic's Usage &
	// Cost Admin API, to reconcile against per-Generation meters.
	mux.HandleFunc("GET /admin/costs", s.admin(s.handleAdminCosts))
	mux.HandleFunc("GET /admin/usage", s.admin(s.handleAdminUsage))

	return s.logged(mux), nil
}

// --- middleware ---

// credHash is the stored form of both credentials: hex SHA-256 of
// "user:secret".
func credHash(userID, secret string) string {
	sum := sha256.Sum256([]byte(userID + ":" + secret))
	return hex.EncodeToString(sum[:])
}

func hashEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 && a != ""
}

type authedHandler func(w http.ResponseWriter, r *http.Request, u store.User)

// auth resolves Basic auth (username + publish token) for the Publishing
// Contract, the Management API, and the Dashboard. The read side does
// not authenticate at all — it lives under /f/{token} (ADR 0008).
func (s *server) auth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, secret, ok := r.BasicAuth()
		if ok && store.ValidID(userID) {
			u, err := s.store.GetUser(r.Context(), userID)
			if err == nil && hashEqual(credHash(userID, secret), u.PublishHash) {
				h(w, r, u)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="podcasting_server"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// feed resolves the {token} path segment to its User. An unknown token
// is a plain 404: capability URLs reveal nothing, valid or not.
func (s *server) feed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.store.GetUserByFeedToken(r.Context(), r.PathValue("token"))
		if err != nil {
			s.fail(w, err)
			return
		}
		h(w, r, u)
	}
}

func (s *server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok {
			got := sha256.Sum256([]byte(token))
			if subtle.ConstantTimeCompare(got[:], s.adminHash[:]) == 1 {
				h(w, r)
				return
			}
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
}

// feedEntries assembles u's Personal Feed: own episodes plus shared-in
// references, muted owners hidden, newest-first. from ("" = all, "me", or
// an owner ID) and filter ("" = all, "mine", "shared") are the Feed
// Variant parameters (ADR 0005).
func (s *server) feedEntries(r *http.Request, u store.User, from, filter string) ([]feedEntry, error) {
	if from == "me" {
		from = u.ID
	}
	entries := []feedEntry{}

	if filter != "shared" && (from == "" || from == u.ID) {
		own, err := s.store.ListEpisodes(r.Context(), u.ID)
		if err != nil {
			return nil, err
		}
		for _, ep := range own {
			entries = append(entries, feedEntry{Episode: ep})
		}
	}

	if filter != "mine" && from != u.ID {
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
	episodes := make([]store.Episode, len(entries))
	for i, e := range entries {
		episodes[i] = e.Episode
	}
	body, err := feed.RSS(u, episodes, s.base(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write(body)
}

// handleAudio serves an enclosure inside the feed's capability
// namespace. The feed's owner may fetch their own episodes and any
// shared into their feed; everything else does not exist.
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

func (s *server) handleCover(w http.ResponseWriter, r *http.Request, u store.User) {
	cover, contentType, err := s.store.OpenCover(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer cover.Close()
	w.Header().Set("Content-Type", contentType)
	// Cacheable: a replaced cover may take up to an hour to reach
	// clients (ADR 0003).
	w.Header().Set("Cache-Control", "public, max-age=3600")
	io.Copy(w, cover)
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

func (s *server) handleHome(w http.ResponseWriter, _ *http.Request) {
	s.render(w, http.StatusOK, s.tmplHome, nil)
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
	s.render(w, http.StatusOK, s.tmplUser, data)
}

func (s *server) renderNotFound(w http.ResponseWriter) {
	s.render(w, http.StatusNotFound, s.tmplNotFound, nil)
}

// render buffers first so a template error can still become a 500.
func (s *server) render(w http.ResponseWriter, status int, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
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
	episodes, err := s.store.ListEpisodes(r.Context(), u.ID)
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
	generations, err := s.dashboardGenerations(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, s.tmplDashboard, struct {
		User            store.User
		FeedPage        string
		Episodes        []store.Episode
		Invites         []inviteView
		GenerateEnabled bool
		Generations     []generationView
		subscribeBox
	}{
		User:            u,
		FeedPage:        "/f/" + u.FeedToken,
		Episodes:        episodes,
		Invites:         pending,
		GenerateEnabled: s.generator != nil,
		Generations:     generations,
		subscribeBox:    s.subscribeBox(r, u),
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
	if err := s.store.SetCover(r.Context(), u.ID, contentType, body); err != nil {
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

// handleShare places an episode from the caller's feed into another
// user's feed. Anyone may share what is in their feed, own or shared —
// forwarding is allowed (ADR 0006).
func (s *server) handleShare(w http.ResponseWriter, r *http.Request, u store.User) {
	ownerID, slug := r.PathValue("owner"), r.PathValue("slug")
	var req struct {
		To string `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.To == u.ID {
		http.Error(w, "cannot share to yourself", http.StatusBadRequest)
		return
	}

	// The episode must be in the caller's feed: their own, or shared in.
	if err := s.inFeed(r, u, ownerID, slug); err != nil {
		s.fail(w, err)
		return
	}
	recipient, err := s.store.GetUser(r.Context(), req.To)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such user: "+req.To, http.StatusNotFound)
			return
		}
		s.fail(w, err)
		return
	}
	if recipient.Blocked(u.ID) {
		http.Error(w, "recipient has blocked you", http.StatusForbidden)
		return
	}
	if recipient.ID == ownerID {
		w.WriteHeader(http.StatusNoContent) // it is their episode already
		return
	}
	if _, err := s.store.GetShare(r.Context(), recipient.ID, ownerID, slug); err == nil {
		w.WriteHeader(http.StatusNoContent) // already in their feed
		return
	}
	err = s.store.AddShare(r.Context(), store.Share{
		UserID:   recipient.ID,
		OwnerID:  ownerID,
		Slug:     slug,
		SharerID: u.ID,
		SharedAt: time.Now().UTC(),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
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

// handleCreateUser provisions a user and returns their credentials —
// shown exactly once, only hashes are stored (ADR 0005).
func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("user")
	if !store.ValidID(id) {
		http.Error(w, "invalid user id (want ^[a-z0-9][a-z0-9._-]*$)", http.StatusBadRequest)
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

	sec, err := issueSecrets()
	if err != nil {
		s.fail(w, err)
		return
	}
	u := store.User{
		ID:          id,
		Title:       req.Title,
		Description: req.Description,
		Language:    req.Language,
		FeedToken:   sec.feed,
		PublishHash: credHash(id, sec.publish),
	}
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]string{
		"id":            id,
		"publish_token": sec.publish,
		"feed_url":      s.feedURL(r, u),
	})
}

// handleRotateCredentials is the recovery path: no email exists, so a
// user who lost their once-shown secrets asks the operator (ADR 0007).
// Both secrets rotate; episodes and shares are untouched, the podcast
// client resubscribes with the new feed URL.
func (s *server) handleRotateCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("user")
	u, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	sec, err := issueSecrets()
	if err != nil {
		s.fail(w, err)
		return
	}
	u.FeedToken = sec.feed
	u.PublishHash = credHash(id, sec.publish)
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{
		"id":            id,
		"publish_token": sec.publish,
		"feed_url":      s.feedURL(r, u),
	})
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

func (s *server) handleRevokeInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	inv, err := s.store.GetInvite(r.Context(), r.PathValue("token"))
	if err != nil || inv.InviterID != u.ID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if inv.RedeemedBy != "" {
		http.Error(w, "already redeemed", http.StatusConflict)
		return
	}
	if err := s.store.DeleteInvite(r.Context(), inv.Token); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// invitePage is the template data for the Redemption page.
type invitePage struct {
	Inviter      string
	EpisodeTitle string
	Username     string
	Error        string
}

// liveInvite loads a redeemable invite or renders the styled 404 — an
// invalid, expired, or spent token looks like any other missing page.
func (s *server) liveInvite(w http.ResponseWriter, r *http.Request) (store.Invite, bool) {
	inv, err := s.store.GetInvite(r.Context(), r.PathValue("token"))
	if err != nil || !inv.Redeemable(time.Now()) {
		s.renderNotFound(w)
		return store.Invite{}, false
	}
	return inv, true
}

func (s *server) invitePageData(r *http.Request, inv store.Invite) invitePage {
	data := invitePage{Inviter: inv.InviterID}
	if inv.OwnerID != "" {
		// A dead payload (owner deleted the episode) hides silently,
		// consistent with share semantics (ADR 0006).
		if ep, err := s.store.GetEpisode(r.Context(), inv.OwnerID, inv.Slug); err == nil {
			data.EpisodeTitle = ep.Title
		}
	}
	return data
}

func (s *server) handleInvitePage(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.liveInvite(w, r)
	if !ok {
		return
	}
	s.render(w, http.StatusOK, s.tmplInvite, s.invitePageData(r, inv))
}

// handleRedeem turns an Invite into a User: the invitee picks their
// username, credentials are issued and shown exactly once, and the
// payload episode (if any) lands as a Share from the inviter (ADR 0007).
func (s *server) handleRedeem(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.liveInvite(w, r)
	if !ok {
		return
	}
	retry := func(status int, msg, username string) {
		data := s.invitePageData(r, inv)
		data.Error, data.Username = msg, username
		s.render(w, status, s.tmplInvite, data)
	}

	username := r.FormValue("username")
	if !store.ValidID(username) {
		retry(http.StatusBadRequest, "That username is not valid: lowercase letters, digits, dots, dashes, underscores.", username)
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
	if err := s.store.RedeemInvite(r.Context(), inv.Token, username); err != nil {
		// Lost a race with another redemption or a revocation.
		s.renderNotFound(w)
		return
	}
	sec, err := issueSecrets()
	if err != nil {
		s.fail(w, err)
		return
	}
	u := store.User{
		ID:          username,
		Title:       username,
		FeedToken:   sec.feed,
		PublishHash: credHash(username, sec.publish),
	}
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}

	sharedTitle := ""
	if inv.OwnerID != "" && inv.OwnerID != username {
		if ep, err := s.store.GetEpisode(r.Context(), inv.OwnerID, inv.Slug); err == nil {
			sharedTitle = ep.Title
			if err := s.store.AddShare(r.Context(), store.Share{
				UserID:   username,
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

	s.render(w, http.StatusOK, s.tmplWelcome, struct {
		Username     string
		PublishToken string
		SharedTitle  string
		subscribeBox
	}{
		Username:     username,
		PublishToken: sec.publish,
		SharedTitle:  sharedTitle,
		subscribeBox: s.subscribeBox(r, u),
	})
}

// --- helpers ---

// secrets is one issue of a user's two generated secrets (ADR 0008).
type secrets struct {
	feed    string // Feed Token: the capability URL segment
	publish string // publish token
}

func issueSecrets() (secrets, error) {
	var sec secrets
	var err error
	if sec.feed, err = randomHex(16); err != nil {
		return secrets{}, err
	}
	if sec.publish, err = randomHex(24); err != nil {
		return secrets{}, err
	}
	return sec, nil
}

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
