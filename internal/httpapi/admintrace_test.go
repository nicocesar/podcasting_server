package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// seedTracedGeneration writes the incident this feature was built for: a
// generation that asked for elevenlabs, got a 402, and fell back.
func seedTracedGeneration(t *testing.T, st *fsstore.Store, user string) {
	t.Helper()
	g := store.Generation{
		UserID: user, ID: "gen1", Topic: "world cup", Template: "news",
		Language: "en", Voice: "female", Provider: "elevenlabs",
		Stage: store.GenDone, TTSEngine: "edge-tts", TTSAttempts: 2,
		EpisodeSlug: "2026-07-22-world-cup",
		CreatedAt:   time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Trace: []store.TraceEntry{
			{
				At: time.Now().UTC(), Level: store.LevelWarn, Stage: store.GenVoicing,
				Event: "tts.fallback", Message: "tts engine failed, trying next",
				Detail: `{"engine":"elevenlabs","requested_provider":"elevenlabs","err":"http 402"}`,
			},
			{
				At: time.Now().UTC(), Level: store.LevelInfo, Stage: store.GenVoicing,
				Event: "tts.selected", Message: "episode voiced",
				Detail: `{"engine":"edge-tts","attempts":2}`,
			},
		},
	}
	if err := st.PutGeneration(context.Background(), g); err != nil {
		t.Fatal(err)
	}
}

// newTraceServer is a server with a real store and no upstream cost API,
// which the trace view does not need.
func newTraceServer(t *testing.T) (*httptest.Server, *fsstore.Store) {
	t.Helper()
	return newEpisodeCostServer(t, "")
}

func TestAdminGenerationTraceRequiresAdmin(t *testing.T) {
	ts, st := newTraceServer(t)
	alice := createUser(t, ts, "alice")
	seedTracedGeneration(t, st, "alice")
	path := ts.URL + "/admin/generations/alice/gen1"

	// A plain logged-in user must not see the admin surface — and gets a
	// 404 rather than a 403, so it does not advertise itself.
	resp := do(t, "GET", path, alice.sessionCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-admin status = %d, want 404", resp.StatusCode)
	}

	// The break-glass token is header-only and no longer opens reporting.
	resp = do(t, "GET", path, "bearer:"+adminToken, nil, "")
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("ADMIN_TOKEN should not open the trace view")
	}

	resp = do(t, "GET", path, "", nil, "")
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("anonymous request reached the trace view")
	}
}

// TestAdminSessionCanProvision covers the webapp case: an existing admin
// promotes a second admin without reaching for the shared token.
func TestAdminSessionCanProvision(t *testing.T) {
	ts, st := newTraceServer(t)
	admin := createAdmin(t, ts, "root")
	bob := createUser(t, ts, "bob")
	seedTracedGeneration(t, st, "bob")
	tracePath := ts.URL + "/admin/generations/bob/gen1"

	// Before promotion bob cannot even see that the surface exists.
	resp := do(t, "GET", tracePath, bob.sessionCreds(), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pre-promotion status = %d, want 404", resp.StatusCode)
	}

	resp = do(t, "POST", ts.URL+"/admin/users/bob/admin", admin.sessionCreds(), nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin could not appoint an admin: %d %s", resp.StatusCode, body)
	}

	// Bob's existing session gains the surface with no re-login: the flag
	// is read from the record on every request, not baked into the cookie.
	resp = do(t, "GET", tracePath, bob.sessionCreds(), nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("post-promotion status = %d, want 200", resp.StatusCode)
	}
}

// TestProvisioningBoundaries pins who may do what. The split is
// deliberate: appointment and creation are reachable from a browser,
// deletion and password reset are not, because either is account takeover
// in a single call.
func TestProvisioningBoundaries(t *testing.T) {
	ts, _ := newTraceServer(t)
	admin := createAdmin(t, ts, "root")
	alice := createUser(t, ts, "alice")
	createUser(t, ts, "victim")

	cases := []struct {
		name       string
		method     string
		path       string
		creds      string
		wantStatus int
	}{
		// Listing is how an operator finds the id to promote, so it must
		// work with only the token, before any admin exists.
		{"token lists users", "GET", "/admin/users", "bearer:" + adminToken, http.StatusOK},
		{"admin session lists users", "GET", "/admin/users", admin.sessionCreds(), http.StatusOK},
		{"plain user cannot list", "GET", "/admin/users", alice.sessionCreds(), http.StatusUnauthorized},
		{"admin session appoints", "POST", "/admin/users/victim/admin", admin.sessionCreds(), http.StatusOK},
		{"admin session creates", "PUT", "/admin/users/newbie", admin.sessionCreds(), http.StatusCreated},
		{"plain user cannot appoint", "POST", "/admin/users/victim/admin", alice.sessionCreds(), http.StatusUnauthorized},
		{"anonymous cannot appoint", "POST", "/admin/users/victim/admin", "", http.StatusUnauthorized},
		// ADR 0010: an API Key never reaches credential management, so a
		// leaked admin key cannot escalate by appointing an admin.
		{"admin API key cannot appoint", "POST", "/admin/users/victim/admin", admin.publishCreds(), http.StatusForbidden},
		// Destructive pair stays token-only.
		{"admin session cannot delete", "DELETE", "/admin/users/victim", admin.sessionCreds(), http.StatusUnauthorized},
		{"admin session cannot reset password", "POST", "/admin/users/victim/password-reset", admin.sessionCreds(), http.StatusUnauthorized},
		{"token still deletes", "DELETE", "/admin/users/victim", "bearer:" + adminToken, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, tc.method, ts.URL+tc.path, tc.creds, nil, "")
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d (%s)", resp.StatusCode, tc.wantStatus, b)
			}
		})
	}
}

func TestAdminGenerationTraceJSON(t *testing.T) {
	ts, st := newTraceServer(t)
	admin := createAdmin(t, ts, "root")
	seedTracedGeneration(t, st, "root")

	resp := do(t, "GET", ts.URL+"/admin/generations/root/gen1", admin.sessionCreds(), nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	var v struct {
		Worst string `json:"worst"`
		Trace []struct {
			Level   string   `json:"level"`
			Event   string   `json:"event"`
			Chips   []string `json:"detail"`
			Notable bool     `json:"notable"`
		} `json:"trace"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Worst != store.LevelWarn {
		t.Errorf("worst = %q, want warn", v.Worst)
	}
	if len(v.Trace) != 2 {
		t.Fatalf("trace = %d rows, want 2", len(v.Trace))
	}
	if !v.Trace[0].Notable {
		t.Error("the warn row is not marked notable")
	}
	// Detail is flattened into sorted k=v chips for display.
	chips := strings.Join(v.Trace[0].Chips, " ")
	for _, want := range []string{"engine=elevenlabs", "requested_provider=elevenlabs", "err=http 402"} {
		if !strings.Contains(chips, want) {
			t.Errorf("chips %q missing %q", chips, want)
		}
	}
	// Counts render as integers, not "2.00".
	if got := strings.Join(v.Trace[1].Chips, " "); !strings.Contains(got, "attempts=2") {
		t.Errorf("chips %q missing attempts=2", got)
	}
}

func TestAdminGenerationTraceHTML(t *testing.T) {
	ts, st := newTraceServer(t)
	admin := createAdmin(t, ts, "root")
	seedTracedGeneration(t, st, "root")

	req, err := http.NewRequest("GET", ts.URL+"/admin/generations/root/gen1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "session", Value: admin.Session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"tts.fallback", "trace-warn", "requested_provider=elevenlabs", "edge-tts"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("page is missing %q", want)
		}
	}
}
