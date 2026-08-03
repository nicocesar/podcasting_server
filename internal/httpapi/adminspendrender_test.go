package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// TestRenderSpendPageForAPerformedStory renders the real Spend page,
// because the unit test around ElevenLabs() proves only that the method
// builds a string — not that the page shows it. The bug being guarded
// here reached production through the template's side of that seam.
func TestRenderSpendPageForAPerformedStory(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[],"has_more":false}`)
	}))
	defer up.Close()
	st, _ := fsstore.New(t.TempDir())
	handler, err := New(Config{
		Store: st, AdminToken: adminToken, SessionSecret: "s",
		Assets:            os.DirFS("../../cmd/server"),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		AnthropicAdminKey: "sk-ant-admin-test", AnthropicAdminBaseURL: up.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()
	admin := createAdmin(t, ts, "chief")
	ctx := context.Background()
	st.UpsertUser(ctx, store.User{ID: "nico", Title: "Nico"})
	st.PutGeneration(ctx, store.Generation{
		UserID: "nico", ID: "g1", Topic: `Titulo "Aventuras en la granja"`,
		Template: "stories-v2", Stage: store.GenDone,
		TTSEngine: "elevenlabs", TTSCharacters: 4321, DialogueRequests: 3,
		MusicMillis: 60000, MusicCalls: 1, SFXGenerated: 4, SFXCacheHits: 6,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	req, _ := http.NewRequest("GET", ts.URL+"/admin/costs", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "session", Value: admin.Session})
	resp, err2 := http.DefaultClient.Do(req)
	if err2 != nil {
		t.Fatal(err2)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	html := string(b)
	for _, want := range []string{"4,321 chars", "3 takes", "1m 00s music", "4 effects", "6 cached", "spend-meter"} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
	// The wrapping class has to reach the cell too, or the long meter
	// string forces the horizontal scrollbar this table clips its topics
	// to avoid — carrying the cost column off the side of a phone.
	if !strings.Contains(html, `class="spend-amount spend-meter muted"`) {
		t.Error("the meter cell is missing its wrapping class")
	}
	// The topic is the one thing on the row that cannot be reconstructed
	// from the other columns, so it must arrive whole — no ellipsis, and
	// no title-attribute stashing of the real text.
	if !strings.Contains(html, `Titulo &#34;Aventuras en la granja&#34;`) {
		t.Error("the topic was not rendered in full")
	}
	if strings.Contains(html, `title="Titulo`) {
		t.Error("the topic is still being hidden behind a tooltip")
	}
}

// TestSpendTableWrappingBeatsTheNowrapRule pins the CSS specificity that
// made the first fix a no-op: the nowrap rule lives on `.spend-table td`
// (0,1,1), so a bare `.spend-meter` class (0,1,0) loses to it silently
// and the cell grows instead of wrapping — which starved the topic
// column down to an ellipsis.
func TestSpendTableWrappingBeatsTheNowrapRule(t *testing.T) {
	css, err := os.ReadFile("../../cmd/server/static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".spend-table td.spend-topic",
		".spend-table td.spend-meter",
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("missing %q: a bare class cannot override .spend-table td", want)
		}
	}
	if strings.Contains(string(css), "text-overflow: ellipsis;\n  cursor: help;") {
		t.Error("the topic is still clipped to an ellipsis")
	}
}
