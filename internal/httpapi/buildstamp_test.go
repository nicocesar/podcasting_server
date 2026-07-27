package httpapi

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// newStampedServer is a test server built the way a deploy builds one:
// a release from version.txt, a commit and a build time from the linker.
func newStampedServer(t *testing.T, version, commit, builtAt string) *httptest.Server {
	t.Helper()
	st, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Store:         st,
		AdminToken:    adminToken,
		SessionSecret: "test-session-secret",
		Assets:        os.DirFS("../../cmd/server"),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:       version,
		Commit:        commit,
		BuiltAt:       builtAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// TestBuildStampShowsAllThree: the release, the commit and the build
// time are three different questions, and until now they were one field
// — Cloud Build overwrote version.txt with the commit, so the
// hand-bumped release never reached production at all.
func TestBuildStampShowsAllThree(t *testing.T) {
	ts := newStampedServer(t, "0.0.8", "9f1a5c2", "2026-07-25T14:30:00Z")
	alice := createUser(t, ts, "alice")

	page := dashboardHTML(t, ts, alice.sessionCreds())
	for _, want := range []string{
		`class="buildstamp"`,
		"0.0.8",
		"9f1a5c2",
		// The machine-readable instant. The browser turns this into
		// "2 days ago" and a local-time tooltip; the server must not
		// try, because it does not know the reader's timezone.
		`<time datetime="2026-07-25T14:30:00Z">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the build stamp is missing %q", want)
		}
	}
}

// TestBuildStampAbsentInALocalBuild: a working tree has no commit and no
// build time, and a stamp that invented them would be worse than none.
func TestBuildStampAbsentInALocalBuild(t *testing.T) {
	ts := newTestServer(t) // no Version, Commit or BuiltAt
	alice := createUser(t, ts, "alice")

	// The class name also appears in the script that formats the time,
	// which early-returns when there is nothing to format — so this
	// asserts on the element, not on the word.
	page := dashboardHTML(t, ts, alice.sessionCreds())
	if strings.Contains(page, `class="buildstamp"`) {
		t.Errorf("an unstamped build still renders a version stamp:\n%s", page)
	}
}

// TestBuildStampSurvivesAPartialStamp: version.txt always ships, so a
// release with no commit (someone built the image outside Cloud Build)
// should still say what it is rather than vanishing.
func TestBuildStampSurvivesAPartialStamp(t *testing.T) {
	ts := newStampedServer(t, "0.0.8", "", "")
	alice := createUser(t, ts, "alice")

	page := dashboardHTML(t, ts, alice.sessionCreds())
	if !strings.Contains(page, "0.0.8") {
		t.Errorf("a release with no commit shows nothing:\n%s", page)
	}
	if strings.Contains(page, "<time datetime=\"\">") {
		t.Error("an empty build time rendered an empty <time> element")
	}
}
