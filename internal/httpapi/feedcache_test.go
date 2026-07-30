package httpapi

// Conditional GET on the feeds. A podcast client refreshes on a timer and
// the archive only grows, so nearly every poll asks a question whose
// answer is "nothing new" — and used to be answered with the whole
// document.

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// getWithETag sends an If-None-Match and reports the status, the ETag
// that came back, and how many bytes of body arrived.
func getWithETag(t *testing.T, url, etag string) (int, string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("ETag"), len(b)
}

func TestFeedConditionalGet(t *testing.T) {
	ts := newTestServer(t)
	a := createUser(t, ts, "alice")
	publishEpisode(t, ts, a, "2026-07-06-morning",
		`{"title":"Morning","description":"The news."}`, "AUDIO").Body.Close()

	code, etag, n := getWithETag(t, a.FeedURL, "")
	if code != http.StatusOK || n == 0 {
		t.Fatalf("first fetch = %d, %d bytes", code, n)
	}
	if etag == "" {
		t.Fatal("no ETag offered, so a client has nothing to revalidate with")
	}
	// The type podcast clients expect, not the text/xml that ServeContent
	// would infer from the .xml name if it were left to guess.
	resp := do(t, "GET", a.FeedURL, "", nil, "")
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Errorf("content-type = %q", ct)
	}
	// Content-Length: the plain Write this replaced went out chunked, so
	// clients could not show size or progress.
	if resp.Header.Get("Content-Length") == "" {
		t.Error("no Content-Length; the response is still chunked")
	}

	// The poll that costs nothing.
	code, _, n = getWithETag(t, a.FeedURL, etag)
	if code != http.StatusNotModified {
		t.Errorf("revalidation = %d, want 304", code)
	}
	if n != 0 {
		t.Errorf("304 carried %d bytes of body, want none", n)
	}

	// A new Episode has to break the validator, or the feed would never
	// be seen to change — the failure worth guarding against here is a
	// 304 that hides new audio.
	publishEpisode(t, ts, a, "2026-07-07-morning",
		`{"title":"Tuesday","description":"More news."}`, "AUDIO2").Body.Close()
	code, next, n := getWithETag(t, a.FeedURL, etag)
	if code != http.StatusOK {
		t.Fatalf("after publishing, revalidation = %d, want 200", code)
	}
	if next == etag {
		t.Error("ETag unchanged after a new episode")
	}
	if !strings.Contains(readAll(t, a.FeedURL), "Tuesday") {
		t.Error("the new episode is not in the feed")
	}
}

// An edit that changes no Episode count still changes the feed. This is
// why the validator hashes the rendered bytes instead of tracking the
// newest Episode's date: a Last-Modified built on that date would answer
// the edited feed with a stale 304, and the correction would never
// arrive.
func TestFeedETagFollowsEdits(t *testing.T) {
	ts := newTestServer(t)
	a := createUser(t, ts, "alice")
	publishEpisode(t, ts, a, "2026-07-06-morning",
		`{"title":"Typo","description":"The news."}`, "AUDIO").Body.Close()

	_, etag, _ := getWithETag(t, a.FeedURL, "")

	// Republish the same slug with a corrected title (ADR 0002).
	publishEpisode(t, ts, a, "2026-07-06-morning",
		`{"title":"Fixed","description":"The news."}`, "AUDIO").Body.Close()

	code, next, _ := getWithETag(t, a.FeedURL, etag)
	if code != http.StatusOK {
		t.Fatalf("edited feed answered %d, want 200 — the correction is invisible", code)
	}
	if next == etag {
		t.Error("ETag unchanged after an edit")
	}
	if body := readAll(t, a.FeedURL); !strings.Contains(body, "Fixed") {
		t.Error("edit missing from the feed")
	}
}

// The Strand Feed is public and gets the same treatment.
func TestStrandFeedConditionalGet(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	alice := createUser(t, ts, "alice")
	airedEpisode(t, ts, st, alice, "chillout-one", "music")

	url := ts.URL + "/strands/music/feed.xml"
	code, etag, n := getWithETag(t, url, "")
	if code != http.StatusOK || n == 0 {
		t.Fatalf("first fetch = %d, %d bytes", code, n)
	}
	if etag == "" {
		t.Fatal("strand feed offers no ETag")
	}
	code, _, n = getWithETag(t, url, etag)
	if code != http.StatusNotModified || n != 0 {
		t.Errorf("revalidation = %d with %d bytes, want 304 and none", code, n)
	}
}

func readAll(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
