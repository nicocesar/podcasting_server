package httpapi

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// TestArtSpecSurvivesAReload: the canon page used to render every art
// field from the title, with the colour and icon pickers reset to "from
// the words". An admin who drew "night talks" in violet was then told,
// on the very next page load, that the art said something else — and the
// next Save would have made that lie true.
func TestArtSpecSurvivesAReload(t *testing.T) {
	ts, st := newStrandServer(t)
	admin := createAdmin(t, ts, "chief")
	postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
		"id": {"talks"}, "title": {"Night Talks"},
		"art_text": {"night talks"}, "accent": {"violet"}, "icon": {"mic"},
	}).Body.Close()

	got, err := st.GetStrand(context.Background(), "talks")
	if err != nil {
		t.Fatal(err)
	}
	if got.ArtText != "night talks" || got.Accent != "violet" || got.Icon != "mic" {
		t.Fatalf("the art spec was not recorded: %+v", got)
	}

	_, page := htmlPage(t, ts.URL+"/admin/strands", admin.sessionCreds())
	for _, want := range []string{
		`value="night talks"`,
		`<option value="violet" selected>`,
		`<option value="mic" selected>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the canon page does not show the stored art spec: missing %q", want)
		}
	}
}

// TestSavingWordingDoesNotRedrawTheArt is what makes one Save button
// safe. Before the Spec was stored there was no way to tell an edit of
// the description from an instruction to redraw.
func TestSavingWordingDoesNotRedrawTheArt(t *testing.T) {
	ts, _ := newStrandServer(t)
	admin := createAdmin(t, ts, "chief")
	postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
		"id": {"talks"}, "title": {"Night Talks"}, "art_text": {"night talks"},
	}).Body.Close()
	_, before := get(t, ts, "/strands/talks/cover")

	// Same art fields, new description: the cover must be byte-identical.
	postForm(t, ts, admin.sessionCreds(), "/admin/strands/talks", url.Values{
		"title": {"Night Talks"}, "description": {"Long conversations after dark."},
		"art_text": {"night talks"},
	}).Body.Close()
	_, after := get(t, ts, "/strands/talks/cover")
	if after != before {
		t.Error("editing the description redrew the cover art")
	}

	// New words on the art: now it must change.
	postForm(t, ts, admin.sessionCreds(), "/admin/strands/talks", url.Values{
		"title": {"Night Talks"}, "description": {"Long conversations after dark."},
		"art_text": {"late night"},
	}).Body.Close()
	_, redrawn := get(t, ts, "/strands/talks/cover")
	if redrawn == before {
		t.Error("changing the words on the art did not redraw it")
	}
}

// TestUploadedArtSurvivesASave is the case that decided the whole shape
// of ADR 0021: a merged Save that always redrew would destroy an
// uploaded cover the first time somebody fixed a typo. An upload clears
// the Spec, and an empty Spec is how the server knows not to.
func TestUploadedArtSurvivesASave(t *testing.T) {
	ts, st := newStrandServer(t)
	admin := createAdmin(t, ts, "chief")
	postForm(t, ts, admin.sessionCreds(), "/admin/strands", url.Values{
		"id": {"music"}, "title": {"Music"},
	}).Body.Close()

	uploadStrandCover(t, ts, admin.sessionCreds(), "music",
		smallJPEG(t, 600, 600), "image/jpeg").Body.Close()

	got, err := st.GetStrand(context.Background(), "music")
	if err != nil {
		t.Fatal(err)
	}
	if got.ArtText != "" || got.Accent != "" || got.Icon != "" {
		t.Errorf("uploading did not clear the art spec: %+v", got)
	}
	if got.CoverType != "image/jpeg" {
		t.Errorf("cover type after upload = %q, want image/jpeg", got.CoverType)
	}
	_, uploaded := get(t, ts, "/strands/music/cover")

	// The form shows the art fields empty against an upload, and posting
	// them back empty must mean "keep my file" rather than "draw the
	// title over it".
	postForm(t, ts, admin.sessionCreds(), "/admin/strands/music", url.Values{
		"title": {"Music"}, "description": {"Anything with a tune."},
		"art_text": {""},
	}).Body.Close()

	_, after := get(t, ts, "/strands/music/cover")
	if after != uploaded {
		t.Error("saving the wording overwrote an uploaded cover")
	}
	if got, _ := st.GetStrand(context.Background(), "music"); got.Description != "Anything with a tune." {
		t.Errorf("the description was not saved: %+v", got)
	}

	// Typing words is still an instruction to redraw, and it says so on
	// the page.
	postForm(t, ts, admin.sessionCreds(), "/admin/strands/music", url.Values{
		"title": {"Music"}, "art_text": {"music"},
	}).Body.Close()
	if _, drawn := get(t, ts, "/strands/music/cover"); drawn == uploaded {
		t.Error("typing words on an uploaded strand did not redraw the art")
	}
}

// TestStrandsWithNoSpecStillWork: every Strand created before ADR 0021
// has an empty Spec and art that came from its title. Nothing migrates
// them, so an empty Spec has to keep meaning what it always meant.
func TestStrandsWithNoSpecStillWork(t *testing.T) {
	ts, st := newStrandServer(t)
	putStrand(t, st, "music", true, false) // straight into the store, no Spec
	admin := createAdmin(t, ts, "chief")

	_, page := htmlPage(t, ts.URL+"/admin/strands", admin.sessionCreds())
	if !strings.Contains(page, `id="strand-music"`) {
		t.Fatalf("a pre-ADR-0021 strand does not render:\n%s", page)
	}

	// Saving it adopts the Spec the form was showing, which is its title.
	postForm(t, ts, admin.sessionCreds(), "/admin/strands/music", url.Values{
		"title": {"Music"}, "art_text": {"music"},
	}).Body.Close()
	got, err := st.GetStrand(context.Background(), "music")
	if err != nil {
		t.Fatal(err)
	}
	if got.ArtText != "music" {
		t.Errorf("saving did not adopt an art spec: %+v", got)
	}
	if got.Dormant() {
		t.Error("saving put an awake strand back to sleep")
	}
}
