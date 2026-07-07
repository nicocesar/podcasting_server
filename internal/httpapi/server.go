// Package httpapi is the HTTP surface of the podcasting server: the
// read-side endpoints AntennaPod consumes (feed, audio, cover) and the
// write-side Publishing Contract + Management API (see docs/adr/0001).
package httpapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/audio"
	"github.com/nicocesar/podcasting_server/internal/feed"
	"github.com/nicocesar/podcasting_server/internal/store"
)

// maxUploadBytes caps write-request bodies. Cloud Run itself caps HTTP/1
// requests at 32 MiB; this is a backstop for local development.
const maxUploadBytes = 256 << 20

type Config struct {
	Store store.Store
	// BaseURL overrides the external base URL used in feed links. When
	// empty, it is derived per-request from Host and X-Forwarded-Proto,
	// which is correct behind Cloud Run.
	BaseURL string
	// ReaderCreds and WriterCreds are "user:password". Reader may only
	// read feeds/audio/covers; Writer may do everything.
	ReaderCreds string
	WriterCreds string
	// Assets holds the "templates" and "static" directories for the
	// Public Surface pages (cmd/server embeds and passes them).
	Assets fs.FS
	Logger *slog.Logger
}

type server struct {
	store      store.Store
	baseURL    string
	readerHash [32]byte
	writerHash [32]byte
	log        *slog.Logger

	tmplHome     *template.Template
	tmplShow     *template.Template
	tmplNotFound *template.Template
}

func New(cfg Config) (http.Handler, error) {
	s := &server{
		store:      cfg.Store,
		baseURL:    strings.TrimSuffix(cfg.BaseURL, "/"),
		readerHash: sha256.Sum256([]byte(cfg.ReaderCreds)),
		writerHash: sha256.Sum256([]byte(cfg.WriterCreds)),
		log:        cfg.Logger,
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
		{&s.tmplShow, []string{"templates/layout.html", "templates/show.html", "templates/fragments/*.html"}},
		{&s.tmplNotFound, []string{"templates/layout.html", "templates/notfound.html"}},
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

	// Public Surface (no auth; ADR 0003): a Show's identity, not its
	// content. The catch-all makes every unmatched path a styled 404.
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /shows/{show}", s.handleShowPage)
	mux.HandleFunc("GET /shows/{show}/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/shows/"+r.PathValue("show"), http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /shows/{show}/cover", s.handleCover)
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheControl("public, max-age=86400", http.FileServerFS(static))))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		s.renderNotFound(w)
	})

	// Read side (AntennaPod).
	mux.HandleFunc("GET /shows/{show}/feed.xml", s.auth(false, s.handleFeed))
	mux.HandleFunc("GET /shows/{show}/episodes/{file}", s.auth(false, s.handleAudio))

	// Write side (Generator + owner).
	mux.HandleFunc("GET /shows", s.auth(true, s.handleListShows))
	mux.HandleFunc("PUT /shows/{show}", s.auth(true, s.handleUpsertShow))
	mux.HandleFunc("DELETE /shows/{show}", s.auth(true, s.handleDeleteShow))
	mux.HandleFunc("PUT /shows/{show}/image", s.auth(true, s.handleSetCover))
	mux.HandleFunc("GET /shows/{show}/episodes", s.auth(true, s.handleListEpisodes))
	mux.HandleFunc("PUT /shows/{show}/episodes/{slug}", s.auth(true, s.handlePublish))
	mux.HandleFunc("DELETE /shows/{show}/episodes/{slug}", s.auth(true, s.handleDeleteEpisode))

	return s.logged(mux), nil
}

// --- middleware ---

func (s *server) auth(needWriter bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok {
			got := sha256.Sum256([]byte(user + ":" + pass))
			isWriter := subtle.ConstantTimeCompare(got[:], s.writerHash[:]) == 1
			isReader := subtle.ConstantTimeCompare(got[:], s.readerHash[:]) == 1
			if isWriter || (!needWriter && isReader) {
				h(w, r)
				return
			}
			if isReader {
				http.Error(w, "writer credentials required", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="podcasting_server"`)
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

func (s *server) handleFeed(w http.ResponseWriter, r *http.Request) {
	showID := r.PathValue("show")
	show, err := s.store.GetShow(r.Context(), showID)
	if err != nil {
		s.fail(w, err)
		return
	}
	episodes, err := s.store.ListEpisodes(r.Context(), showID)
	if err != nil {
		s.fail(w, err)
		return
	}
	body, err := feed.RSS(show, episodes, s.base(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write(body)
}

func (s *server) handleAudio(w http.ResponseWriter, r *http.Request) {
	slug, ok := strings.CutSuffix(r.PathValue("file"), ".mp3")
	if !ok || !store.ValidID(slug) {
		http.NotFound(w, r)
		return
	}
	audio, err := s.store.OpenAudio(r.Context(), r.PathValue("show"), slug)
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

func (s *server) handleCover(w http.ResponseWriter, r *http.Request) {
	cover, contentType, err := s.store.OpenCover(r.Context(), r.PathValue("show"))
	if err != nil {
		s.fail(w, err)
		return
	}
	defer cover.Close()
	w.Header().Set("Content-Type", contentType)
	// Public and cacheable: a replaced cover may take up to an hour to
	// reach clients (ADR 0003).
	w.Header().Set("Cache-Control", "public, max-age=3600")
	io.Copy(w, cover)
}

// --- Public Surface pages (ADR 0003) ---

func (s *server) handleHome(w http.ResponseWriter, _ *http.Request) {
	s.render(w, http.StatusOK, s.tmplHome, nil)
}

func (s *server) handleShowPage(w http.ResponseWriter, r *http.Request) {
	show, err := s.store.GetShow(r.Context(), r.PathValue("show"))
	if errors.Is(err, store.ErrNotFound) {
		s.renderNotFound(w)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	data := struct {
		Show     store.Show
		FeedURL  string
		CoverURL string
	}{
		Show:    show,
		FeedURL: s.base(r) + "/shows/" + show.ID + "/feed.xml",
	}
	if show.CoverType != "" {
		data.CoverURL = "/shows/" + show.ID + "/cover"
	}
	s.render(w, http.StatusOK, s.tmplShow, data)
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

// --- write side ---

func (s *server) handleListShows(w http.ResponseWriter, r *http.Request) {
	shows, err := s.store.ListShows(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, shows)
}

func (s *server) handleUpsertShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("show")
	if !store.ValidID(id) {
		http.Error(w, "invalid show id (want ^[a-z0-9][a-z0-9._-]*$)", http.StatusBadRequest)
		return
	}
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
	show := store.Show{ID: id, Title: req.Title, Description: req.Description, Language: req.Language}
	status := http.StatusCreated
	if existing, err := s.store.GetShow(r.Context(), id); err == nil {
		show.CoverType = existing.CoverType // upsert must not drop the cover
		status = http.StatusOK
	}
	if err := s.store.UpsertShow(r.Context(), show); err != nil {
		s.fail(w, err)
		return
	}
	show, _ = s.store.GetShow(r.Context(), id)
	s.writeJSON(w, status, show)
}

func (s *server) handleDeleteShow(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteShow(r.Context(), r.PathValue("show")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleSetCover(w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	if contentType != "image/jpeg" && contentType != "image/png" {
		http.Error(w, "Content-Type must be image/jpeg or image/png", http.StatusUnsupportedMediaType)
		return
	}
	body := http.MaxBytesReader(w, r.Body, 8<<20)
	err := s.store.SetCover(r.Context(), r.PathValue("show"), contentType, body)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleListEpisodes(w http.ResponseWriter, r *http.Request) {
	episodes, err := s.store.ListEpisodes(r.Context(), r.PathValue("show"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, episodes)
}

// handlePublish is the Publishing Contract: multipart/form-data with a
// "metadata" JSON field and an "audio" file field. Publishing an existing
// slug replaces the episode (ADR 0002).
func (s *server) handlePublish(w http.ResponseWriter, r *http.Request) {
	showID, slug := r.PathValue("show"), r.PathValue("slug")
	if !store.ValidID(slug) {
		http.Error(w, "invalid slug (want ^[a-z0-9][a-z0-9._-]*$)", http.StatusBadRequest)
		return
	}
	if _, err := s.store.GetShow(r.Context(), showID); err != nil {
		s.fail(w, err)
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
			s.log.Warn("could not estimate duration", "show", showID, "slug", slug, "err", err)
		}
		if _, err := audioFile.Seek(0, io.SeekStart); err != nil {
			s.fail(w, err)
			return
		}
	}

	_, replaced := s.episodeExists(r, showID, slug)
	ep, err := s.store.UpsertEpisode(r.Context(), store.Episode{
		ShowID:      showID,
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

func (s *server) episodeExists(r *http.Request, showID, slug string) (store.Episode, bool) {
	ep, err := s.store.GetEpisode(r.Context(), showID, slug)
	return ep, err == nil
}

func (s *server) handleDeleteEpisode(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteEpisode(r.Context(), r.PathValue("show"), r.PathValue("slug"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

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
