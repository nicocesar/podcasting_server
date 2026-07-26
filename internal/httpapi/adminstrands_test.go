package httpapi

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// uploadStrandCover posts an image to the canon page's cover form.
func uploadStrandCover(t *testing.T, ts *httptest.Server, creds, strand string, img []byte, contentType string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="cover"; filename="art.jpg"`)
	h.Set("Content-Type", contentType)
	fw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(img)
	mw.Close()
	return sendNoRedirect(t, "POST", ts.URL+"/admin/strands/"+strand+"/cover", creds, &buf, mw.FormDataContentType())
}

// TestAdminCreatesAStrandDormant: a new canon entry has no art, so it is
// dormant and nothing may be aired into it yet (ADR 0017).
func TestAdminCreatesAStrandDormant(t *testing.T) {
	ts, st := newStrandServer(t)
	admin := createAdmin(t, ts, "chief")

	resp := postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
		"id": {"tech-news"}, "title": {"Tech News"}, "description": {"What happened."},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: %d, want 303", resp.StatusCode)
	}
	got, err := st.GetStrand(context.Background(), "tech-news")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Tech News" || got.Description != "What happened." {
		t.Errorf("stored = %+v", got)
	}
	if !got.Dormant() {
		t.Error("a strand with no cover art must be dormant")
	}
	// Dormant means invisible in public, and unairable.
	if resp, _ := get(t, ts, "/strands/tech-news"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("dormant strand page: %d, want 404", resp.StatusCode)
	}
}

// TestAdminCoverWakesAStrand: uploading art is what makes a strand real,
// and it produces both derivatives the rest of the app expects.
func TestAdminCoverWakesAStrand(t *testing.T) {
	ts, st := newStrandServer(t)
	admin := createAdmin(t, ts, "chief")
	postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
		"id": {"music"}, "title": {"Music"},
	}).Body.Close()

	resp := uploadStrandCover(t, ts, admin.sessionCreds(), "music", smallJPEG(t, 600, 600), "image/jpeg")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("cover upload: %d %s", resp.StatusCode, b)
	}
	got, err := st.GetStrand(context.Background(), "music")
	if err != nil {
		t.Fatal(err)
	}
	if got.Dormant() {
		t.Fatal("the strand is still dormant after an upload")
	}

	// Both derivatives serve, and the page is public now.
	for _, path := range []string{"/strands/music/cover", "/strands/music/cover?s=thumb"} {
		r, body := get(t, ts, path)
		if r.StatusCode != http.StatusOK || len(body) == 0 {
			t.Errorf("GET %s: %d, %d bytes", path, r.StatusCode, len(body))
		}
	}
	if r, _ := get(t, ts, "/strands/music"); r.StatusCode != http.StatusOK {
		t.Errorf("strand page after art: %d", r.StatusCode)
	}
}

// TestAdminCoverRejectsNonImages: coverart.Process decodes, so a bad
// upload must fail loudly rather than storing bytes nothing can render.
func TestAdminCoverRejectsNonImages(t *testing.T) {
	ts, _ := newStrandServer(t)
	admin := createAdmin(t, ts, "chief")
	postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
		"id": {"music"}, "title": {"Music"},
	}).Body.Close()

	resp := uploadStrandCover(t, ts, admin.sessionCreds(), "music", []byte("not an image"), "image/gif")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("gif upload: %d, want 415", resp.StatusCode)
	}
}

// TestAdminStrandValidation: the id is a permanent public URL segment
// and shares the namespace with usernames, so the rules are the strict
// ones.
func TestAdminStrandValidation(t *testing.T) {
	ts, st := newStrandServer(t)
	admin := createAdmin(t, ts, "chief")

	for _, tc := range []struct {
		name string
		id   string
		want int
	}{
		{"uppercase", "TechNews", http.StatusUnprocessableEntity},
		{"underscore", "tech_news", http.StatusUnprocessableEntity},
		{"reserved", "admin", http.StatusUnprocessableEntity},
		{"too short", "ab", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
				"id": {tc.id}, "title": {"Whatever"},
			})
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("create %q: %d, want %d", tc.id, resp.StatusCode, tc.want)
			}
		})
	}

	// And an id cannot be claimed twice.
	postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
		"id": {"music"}, "title": {"Music"},
	}).Body.Close()
	dup := postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
		"id": {"music"}, "title": {"Something else"},
	})
	defer dup.Body.Close()
	if dup.StatusCode != http.StatusConflict {
		t.Errorf("duplicate id: %d, want 409", dup.StatusCode)
	}
	if got, _ := st.GetStrand(context.Background(), "music"); got.Title != "Music" {
		t.Errorf("the duplicate overwrote the original: %+v", got)
	}
}

// TestAdminRetireNotDelete: once anything has aired on a Strand it can
// only be retired. Deleting would reach into other people's publishing
// decisions and break links they have handed out (ADR 0017).
func TestAdminRetireNotDelete(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	admin := createAdmin(t, ts, "chief")
	alice := createUser(t, ts, "alice")
	airedEpisode(t, ts, st, alice, "ep1", "music")

	del := postForm(t, ts, admin.sessionCreds(), "/admin/strands/music/delete", url.Values{})
	del.Body.Close()
	if del.StatusCode != http.StatusConflict {
		t.Fatalf("delete an aired-on strand: %d, want 409", del.StatusCode)
	}
	if _, err := st.GetStrand(context.Background(), "music"); err != nil {
		t.Fatal("the strand was deleted anyway")
	}

	retire := postForm(t, ts, admin.sessionCreds(), "/admin/strands/music/retire", url.Values{})
	retire.Body.Close()
	got, err := st.GetStrand(context.Background(), "music")
	if err != nil || !got.Retired {
		t.Fatalf("retire: %+v, %v", got, err)
	}

	back := postForm(t, ts, admin.sessionCreds(), "/admin/strands/music/unretire", url.Values{})
	back.Body.Close()
	if got, _ = st.GetStrand(context.Background(), "music"); got.Retired {
		t.Error("unretire did not bring it back")
	}
}

// TestAdminDeletesAnUnusedStrand: the five-minute-old mistake is the one
// case deletion exists for.
func TestAdminDeletesAnUnusedStrand(t *testing.T) {
	ts, st := newStrandServer(t)
	admin := createAdmin(t, ts, "chief")
	postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
		"id": {"oops"}, "title": {"Oops"},
	}).Body.Close()

	resp := postForm(t, ts, admin.sessionCreds(), "/admin/strands/oops/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete an unused strand: %d, want 303", resp.StatusCode)
	}
	if _, err := st.GetStrand(context.Background(), "oops"); err == nil {
		t.Fatal("the strand survived")
	}
}

// TestAdminCanonIsAdminOnly: the canon decides what the whole public
// side can ever be about.
func TestAdminCanonIsAdminOnly(t *testing.T) {
	ts, st := newStrandServer(t)
	alice := createUser(t, ts, "alice")

	page := do(t, "GET", ts.URL+"/admin/strands", alice.sessionCreds(), nil, "")
	page.Body.Close()
	if page.StatusCode != http.StatusNotFound {
		t.Errorf("GET /admin/strands as a user: %d, want 404", page.StatusCode)
	}
	create := postForm(t, ts, alice.sessionCreds(), "/admin/strands", url.Values{
		"id": {"mine"}, "title": {"Mine"},
	})
	create.Body.Close()
	if create.StatusCode != http.StatusNotFound {
		t.Errorf("create as a user: %d, want 404", create.StatusCode)
	}
	if _, err := st.GetStrand(context.Background(), "mine"); err == nil {
		t.Fatal("a non-admin added to the canon")
	}
}

// TestAdminPageShowsTheCanon: the screen an operator actually reads.
func TestAdminPageShowsTheCanon(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false)
	putStrand(t, st, "sleepy", false, false)
	admin := createAdmin(t, ts, "chief")

	resp := do(t, "GET", ts.URL+"/admin/strands", admin.sessionCreds(), nil, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/strands: %d", resp.StatusCode)
	}
	for _, want := range []string{"music", "sleepy", "dormant, waiting for cover art"} {
		if !strings.Contains(page, want) {
			t.Errorf("admin page missing %q", want)
		}
	}
}

// TestSeedStrandsMatchTheValidator guards the four a fresh install
// writes unreviewed: they go straight into the store, so an id the
// admin page would reject must not be one of them.
func TestSeedStrandsMatchTheValidator(t *testing.T) {
	for _, s := range store.SeedStrands() {
		if err := store.ValidateStrand(s); err != nil {
			t.Errorf("seed strand %q would be rejected by the admin page: %v", s.ID, err)
		}
	}
}
