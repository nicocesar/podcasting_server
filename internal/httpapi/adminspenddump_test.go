package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
	"github.com/nicocesar/podcasting_server/internal/store/fsstore"
)

// TestDumpSpendPage writes the rendered Spend page to DUMP_TO so it can
// be looked at in a browser. Skipped unless that is set — it asserts
// nothing, and exists because the layout bugs in this table (a topic
// clipped to "Titu…", meters running off the right edge, a meter wrapping
// between "1m 00s" and "music") are all invisible to a test that only
// greps the HTML for substrings. Serve the file next to a copy of
// cmd/server/static and open it at a phone width.
func TestDumpSpendPage(t *testing.T) {
	out := os.Getenv("DUMP_TO")
	if out == "" {
		t.Skip("set DUMP_TO to write the rendered page somewhere")
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[],"has_more":false}`)
	}))
	defer up.Close()
	st, _ := fsstore.New(t.TempDir())
	handler, _ := New(Config{
		Store: st, AdminToken: adminToken, SessionSecret: "s",
		Assets:            os.DirFS("../../cmd/server"),
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		AnthropicAdminKey: "sk-ant-admin-test", AnthropicAdminBaseURL: up.URL,
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	admin := createAdmin(t, ts, "chief")
	ctx := context.Background()
	st.UpsertUser(ctx, store.User{ID: "nico", Title: "Nico"})

	now := time.Now().UTC()
	rows := []struct {
		id, topic                      string
		chars, takes, sfx, hits, music int
	}{
		{"g1", "Una historia sobre un pato y un chancho que se hacen amigos en un granero vacío", 2062, 15, 1, 12, 60000},
		{"g2", "Today's tech news", 0, 0, 0, 0, 0},
		{"g3", `Titulo "Aventuras en la granja". Palabras en español para practicar antes de dormir`, 2079, 21, 7, 8, 60000},
		{"g4", "Eventos en Buenos Aires esta semana", 0, 0, 0, 0, 0},
		{"g5", "rain on a window, late evening", 0, 0, 0, 0, 1500000},
	}
	for i, r := range rows {
		g := store.Generation{
			UserID: "nico", ID: r.id, Topic: r.topic, Stage: store.GenDone,
			CreatedAt: now.Add(-time.Duration(i) * time.Hour), UpdatedAt: now,
		}
		if r.chars > 0 {
			g.TTSEngine, g.TTSCharacters, g.DialogueRequests = "elevenlabs", r.chars, r.takes
			g.SFXGenerated, g.SFXCacheHits = r.sfx, r.hits
		}
		if r.music > 0 {
			g.MusicMillis, g.MusicCalls = r.music, 1
			if r.chars == 0 {
				g.MusicCalls = 3
			}
		}
		st.PutGeneration(ctx, g)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/admin/costs", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "session", Value: admin.Session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	os.WriteFile(out, b, 0o644)
	t.Logf("wrote %d bytes to %s", len(b), out)
}
