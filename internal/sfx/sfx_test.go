package sfx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// vendor stands in for /v1/sound-generation, counting how often it is
// actually reached — which is the number the cache exists to hold down.
type vendor struct {
	calls int
	body  []byte
}

func (v *vendor) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.calls++
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(v.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, v *vendor) *Client {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := New("test-key", st)
	if err != nil {
		t.Fatal(err)
	}
	c.baseURL = v.server(t).URL
	return c
}

func TestRenderCachesAcrossEpisodes(t *testing.T) {
	// The point of the cache is not only cost: an effect regenerated every
	// episode is a slightly different effect every episode, so the duck a
	// listener met last week would not be the duck they meet this week.
	v := &vendor{body: []byte("quack-audio")}
	c := newTestClient(t, v)
	ctx := context.Background()

	first, err := c.Render(ctx, Cue{Name: "duck_quack"})
	if err != nil {
		t.Fatal(err)
	}
	if first.CacheHit {
		t.Error("the first render reported a cache hit")
	}
	if v.calls != 1 {
		t.Fatalf("vendor calls = %d, want 1", v.calls)
	}

	second, err := c.Render(ctx, Cue{Name: "duck_quack"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.CacheHit {
		t.Error("the second render did not hit the cache")
	}
	if v.calls != 1 {
		t.Errorf("vendor calls = %d, want the second render served from cache", v.calls)
	}
	if string(first.Audio) != string(second.Audio) {
		t.Error("the cached effect differs from the one that was stored")
	}
}

func TestRenderCachesFreeformCuesByMeaning(t *testing.T) {
	// Two spellings of the same request are one effect, so a second story
	// asking for the same sound in different words does not pay twice.
	v := &vendor{body: []byte("gate-audio")}
	c := newTestClient(t, v)
	ctx := context.Background()

	if _, err := c.Render(ctx, Cue{Name: "a rusty gate, slow"}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Render(ctx, Cue{Name: "  A Rusty  Gate, Slow "})
	if err != nil {
		t.Fatal(err)
	}
	if !res.CacheHit {
		t.Error("a differently-spelled identical cue missed the cache")
	}
	if v.calls != 1 {
		t.Errorf("vendor calls = %d, want 1", v.calls)
	}
}

func TestCacheKey(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cue        string
		prompt     string
		durationMS int
		wantPrefix string
		wantSame   bool // same key as the previous case
	}{
		{name: "a library cue is legible in the bucket", cue: "duck_quack", prompt: "x", durationMS: 1500, wantPrefix: "sfx/v1/lib/duck_quack-1500ms"},
		{name: "a freeform cue is hashed", cue: "a rusty gate", prompt: "a rusty gate", durationMS: 2000, wantPrefix: "sfx/v1/gen/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CacheKey(tc.cue, tc.prompt, tc.durationMS)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("CacheKey = %q, want prefix %q", got, tc.wantPrefix)
			}
		})
	}

	// Duration is part of the identity: the same sound at a different
	// length is a different rendering and must not be served from one key.
	if CacheKey("a gate", "a gate", 1000) == CacheKey("a gate", "a gate", 2000) {
		t.Error("cues of different durations share a cache key")
	}
	// A library cue keeps its curated prompt out of the key, so editing a
	// comment in the library does not orphan every stored effect.
	if CacheKey("duck_quack", "one prompt", 1500) != CacheKey("duck_quack", "another prompt", 1500) {
		t.Error("a library cue's key depends on its prompt text")
	}
}

func TestResolveUsesTheCuratedEntry(t *testing.T) {
	c := &Client{}
	prompt, dur := c.resolve(Cue{Name: "duck_quack"})
	if !strings.Contains(prompt, "duck") {
		t.Errorf("library prompt = %q, want the curated wording", prompt)
	}
	lib, _ := lookup("duck_quack")
	if dur != lib.DurationMS {
		t.Errorf("duration = %d, want the curated %d", dur, lib.DurationMS)
	}

	// An unknown cue is its own prompt, at the default length.
	prompt, dur = c.resolve(Cue{Name: "a rusty gate"})
	if prompt != "a rusty gate" {
		t.Errorf("freeform prompt = %q", prompt)
	}
	if dur != DefaultDurationMS {
		t.Errorf("freeform duration = %d, want %d", dur, DefaultDurationMS)
	}
}

func TestResolveClampsToTheVendorBounds(t *testing.T) {
	c := &Client{}
	if _, dur := c.resolve(Cue{Name: "x", DurationMS: 1}); dur != MinDurationMS {
		t.Errorf("under-short cue = %dms, want clamped to %d", dur, MinDurationMS)
	}
	if _, dur := c.resolve(Cue{Name: "x", DurationMS: 999_999}); dur != MaxDurationMS {
		t.Errorf("over-long cue = %dms, want clamped to %d", dur, MaxDurationMS)
	}
}

func TestRenderSurfacesAVendorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"detail":"quota exhausted"}`)
	}))
	defer srv.Close()

	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := New("test-key", st)
	if err != nil {
		t.Fatal(err)
	}
	c.baseURL = srv.URL

	if _, err := c.Render(context.Background(), Cue{Name: "duck_quack"}); err == nil {
		t.Fatal("want an error from a 402")
	}
}

func TestLibraryNamesAreUnique(t *testing.T) {
	// The names go into a tool-schema enum and into cache paths; a
	// duplicate would make one of them unreachable.
	seen := map[string]bool{}
	for _, n := range LibraryNames() {
		if seen[n] {
			t.Errorf("duplicate library cue %q", n)
		}
		seen[n] = true
	}
	if len(seen) == 0 {
		t.Error("the cue library is empty")
	}
}
