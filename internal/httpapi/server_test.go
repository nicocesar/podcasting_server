package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

const (
	readerCreds = "phone:readpass"
	writerCreds = "generator:writepass"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:       st,
		ReaderCreds: readerCreds,
		WriterCreds: writerCreds,
		// The real embedded assets, via the filesystem.
		Assets: os.DirFS("../../cmd/server"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, method, url, creds string, body io.Reader, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if creds != "" {
		user, pass, _ := strings.Cut(creds, ":")
		req.SetBasicAuth(user, pass)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func createShow(t *testing.T, ts *httptest.Server, id string) {
	t.Helper()
	resp := do(t, "PUT", ts.URL+"/shows/"+id, writerCreds,
		strings.NewReader(`{"title":"AI News","description":"Daily AI briefings"}`), "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create show: %d", resp.StatusCode)
	}
}

func publishEpisode(t *testing.T, ts *httptest.Server, show, slug, metadata, audio string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("metadata", metadata); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("audio", slug+".mp3")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte(audio))
	mw.Close()
	return do(t, "PUT", ts.URL+"/shows/"+show+"/episodes/"+slug, writerCreds, &buf, mw.FormDataContentType())
}

func TestAuth(t *testing.T) {
	ts := newTestServer(t)
	createShow(t, ts, "ai-news")

	cases := []struct {
		name, method, path, creds string
		want                      int
	}{
		{"no creds on feed", "GET", "/shows/ai-news/feed.xml", "", 401},
		{"bad creds on feed", "GET", "/shows/ai-news/feed.xml", "phone:wrong", 401},
		{"reader reads feed", "GET", "/shows/ai-news/feed.xml", readerCreds, 200},
		{"writer reads feed", "GET", "/shows/ai-news/feed.xml", writerCreds, 200},
		{"reader cannot list shows", "GET", "/shows", readerCreds, 403},
		{"writer lists shows", "GET", "/shows", writerCreds, 200},
		{"no creds on healthz ok", "GET", "/healthz", "", 200},
	}
	for _, c := range cases {
		resp := do(t, c.method, ts.URL+c.path, c.creds, nil, "")
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: got %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

func TestPublicSurface(t *testing.T) {
	ts := newTestServer(t)
	createShow(t, ts, "ai-news")
	resp := do(t, "PUT", ts.URL+"/shows/ai-news/image", writerCreds, strings.NewReader("JPEG"), "image/jpeg")
	resp.Body.Close()
	resp = publishEpisode(t, ts, "ai-news", "2026-07-06-morning",
		`{"title":"Secret Episode Title","description":"Secret summary."}`, "MP3")
	resp.Body.Close()

	// Public Surface: no credentials needed (ADR 0003).
	for path, want := range map[string]int{
		"/":                    200,
		"/shows/ai-news":       200,
		"/shows/ai-news/cover": 200,
		"/static/style.css":    200,
		"/shows/no-such-show":  404,
		"/no/such/path":        404,
	} {
		resp := do(t, "GET", ts.URL+path, "", nil, "")
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s without creds: got %d, want %d", path, resp.StatusCode, want)
		}
	}

	// The Show Page exposes identity, never Episode data.
	resp = do(t, "GET", ts.URL+"/shows/ai-news", "", nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	for _, want := range []string{"AI News", "/shows/ai-news/feed.xml", "/shows/ai-news/cover"} {
		if !strings.Contains(page, want) {
			t.Errorf("show page missing %q:\n%s", want, page)
		}
	}
	for _, leak := range []string{"Secret Episode Title", "Secret summary", "2026-07-06-morning"} {
		if strings.Contains(page, leak) {
			t.Errorf("show page leaks episode data %q:\n%s", leak, page)
		}
	}

	// The landing page enumerates nothing.
	resp = do(t, "GET", ts.URL+"/", "", nil, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "ai-news") {
		t.Errorf("landing page lists shows:\n%s", body)
	}

	// Content stays authenticated.
	for _, path := range []string{
		"/shows/ai-news/feed.xml",
		"/shows/ai-news/episodes/2026-07-06-morning.mp3",
	} {
		resp := do(t, "GET", ts.URL+path, "", nil, "")
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("GET %s without creds: got %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestPublishFeedAndDownload(t *testing.T) {
	ts := newTestServer(t)
	createShow(t, ts, "ai-news")

	meta := `{"title":"Morning Briefing","description":"The news.","published_at":"2026-07-06T08:00:00Z","duration_seconds":300}`
	resp := publishEpisode(t, ts, "ai-news", "2026-07-06-morning", meta, "FAKEMP3BYTES")
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("publish: %d %s", resp.StatusCode, b)
	}
	var ep struct {
		Slug      string `json:"slug"`
		AudioSize int64  `json:"audio_size"`
	}
	json.NewDecoder(resp.Body).Decode(&ep)
	resp.Body.Close()
	if ep.Slug != "2026-07-06-morning" || ep.AudioSize != int64(len("FAKEMP3BYTES")) {
		t.Fatalf("unexpected episode: %+v", ep)
	}

	// Republish same slug → 200 (replace), not a new episode.
	resp = publishEpisode(t, ts, "ai-news", "2026-07-06-morning", meta, "NEWBYTES")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("republish: got %d, want 200", resp.StatusCode)
	}

	// Feed contains the item exactly once, with enclosure and guid.
	resp = do(t, "GET", ts.URL+"/shows/ai-news/feed.xml", readerCreds, nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	xml := string(body)
	if resp.StatusCode != 200 {
		t.Fatalf("feed: %d", resp.StatusCode)
	}
	for _, want := range []string{
		"<title>Morning Briefing</title>",
		`/shows/ai-news/episodes/2026-07-06-morning.mp3"`,
		`length="8"`, // len("NEWBYTES")
		`type="audio/mpeg"`,
		`isPermaLink="false"`,
		"ai-news/2026-07-06-morning",
		"<itunes:duration>300</itunes:duration>",
		"Mon, 06 Jul 2026 08:00:00 +0000",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("feed missing %q\n%s", want, xml)
		}
	}
	if strings.Count(xml, "<item>") != 1 {
		t.Errorf("feed should have exactly 1 item:\n%s", xml)
	}

	// Download the audio.
	resp = do(t, "GET", ts.URL+"/shows/ai-news/episodes/2026-07-06-morning.mp3", readerCreds, nil, "")
	audio, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(audio) != "NEWBYTES" {
		t.Fatalf("download: %d %q", resp.StatusCode, audio)
	}

	// Range requests must work (podcast clients resume downloads).
	req, _ := http.NewRequest("GET", ts.URL+"/shows/ai-news/episodes/2026-07-06-morning.mp3", nil)
	req.SetBasicAuth("phone", "readpass")
	req.Header.Set("Range", "bytes=3-")
	rangeResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	partial, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || string(partial) != "BYTES" {
		t.Fatalf("range: %d %q", rangeResp.StatusCode, partial)
	}
}

func TestPublishValidation(t *testing.T) {
	ts := newTestServer(t)
	createShow(t, ts, "ai-news")

	// Unknown show → 404, never auto-created.
	resp := publishEpisode(t, ts, "no-such-show", "2026-07-06-morning", `{"title":"x"}`, "a")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("publish to missing show: got %d, want 404", resp.StatusCode)
	}

	// Invalid slug → 400.
	resp = publishEpisode(t, ts, "ai-news", "Bad_Slug!", `{"title":"x"}`, "a")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid slug: got %d, want 400", resp.StatusCode)
	}

	// Missing title → 400.
	resp = publishEpisode(t, ts, "ai-news", "2026-07-06-morning", `{"description":"no title"}`, "a")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing title: got %d, want 400", resp.StatusCode)
	}
}

func TestCoverRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	createShow(t, ts, "ai-news")

	resp := do(t, "PUT", ts.URL+"/shows/ai-news/image", writerCreds, strings.NewReader("JPEGBYTES"), "image/jpeg")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set cover: %d", resp.StatusCode)
	}

	// Feed advertises the cover.
	resp = do(t, "GET", ts.URL+"/shows/ai-news/feed.xml", readerCreds, nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "/shows/ai-news/cover") {
		t.Errorf("feed missing itunes:image:\n%s", body)
	}

	resp = do(t, "GET", ts.URL+"/shows/ai-news/cover", readerCreds, nil, "")
	img, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(img) != "JPEGBYTES" || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("get cover: %d %q %q", resp.StatusCode, img, resp.Header.Get("Content-Type"))
	}

	// Wrong content type rejected.
	resp = do(t, "PUT", ts.URL+"/shows/ai-news/image", writerCreds, strings.NewReader("GIF"), "image/gif")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("gif cover: got %d, want 415", resp.StatusCode)
	}
}

func TestShowUpsertKeepsCover(t *testing.T) {
	ts := newTestServer(t)
	createShow(t, ts, "ai-news")
	resp := do(t, "PUT", ts.URL+"/shows/ai-news/image", writerCreds, strings.NewReader("JPEG"), "image/jpeg")
	resp.Body.Close()

	// Re-PUT the show metadata; cover must survive.
	resp = do(t, "PUT", ts.URL+"/shows/ai-news", writerCreds,
		strings.NewReader(`{"title":"AI News v2"}`), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-put show: %d", resp.StatusCode)
	}
	var show struct {
		Title     string `json:"title"`
		CoverType string `json:"cover_type"`
	}
	json.NewDecoder(resp.Body).Decode(&show)
	resp.Body.Close()
	if show.Title != "AI News v2" || show.CoverType != "image/jpeg" {
		t.Fatalf("cover dropped on upsert: %+v", show)
	}
}
