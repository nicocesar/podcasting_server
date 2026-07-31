package httpapi

// The admin canon page (ADR 0017). The canon is stored data rather than
// a Go constant so a deployment that is not ours can name its own
// subjects without forking, which means somebody needs a screen to keep
// it on — this is that screen.
//
// It is deliberately small: create, edit the words, draw or upload the
// art, retire. There is no rename, because a Strand's id addresses its
// public feed and renaming it would silently kill every subscription.

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicocesar/podcasting_server/internal/coverart"
	"github.com/nicocesar/podcasting_server/internal/store"
)

// adminStrandRow is one canon entry on the admin page.
type adminStrandRow struct {
	store.Strand
	Airings int
	// Deletable is true only for a Strand nothing has ever aired on —
	// a mistake made five minutes ago. Everything else retires.
	Deletable bool
	// ArtText, Accent and Icon are the Art Spec as stored, so the form
	// shows what the art actually says rather than guessing from the
	// title. ArtText falls back to the title only where there is no
	// Spec at all — a Strand created before ADR 0021, or one whose art
	// was uploaded, where the title is the honest default for the words
	// a redraw would use.
	ArtText string
	Accent  string
	Icon    string
	// Uploaded marks a cover that came from a file rather than from
	// words: art with no Spec behind it. Leaving the art fields alone on
	// such a Strand keeps the upload (ADR 0021).
	Uploaded bool
}

type adminStrandsPage struct {
	User    store.User
	Strands []adminStrandRow
	Error   string
	// Accents and Icons are the pickers for generated art.
	Accents []string
	Icons   []string
	// ReturnTo is this page, so an action lands back on the strand it
	// touched rather than at the top of the canon (ADR 0022).
	ReturnTo string
}

func (s *server) handleAdminStrands(w http.ResponseWriter, r *http.Request, u store.User) {
	s.renderAdminStrands(w, r, u, "", http.StatusOK)
}

// handleAdminIndex is the door the chrome's Admin link opens: the canon
// and Spend, and a pointer to where moderation actually lives. It reads
// almost nothing and decides nothing — every surface it names guards
// itself.
func (s *server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	// Two exceptions to "reads nothing". The ElevenLabs balance, so a low
	// one is seen on the way past rather than found later in a failed
	// generation's trace; it is cached, so it costs nothing most of the
	// time. And the last Tick, because a deployment with no scheduler job
	// pointed at it is invisible from every other surface (ADR 0028).
	s.render(w, r, http.StatusOK, s.tmplAdmin, struct {
		CostsConfigured bool
		Credits         creditView
		Tick            tickView
	}{
		CostsConfigured: s.adminAPI != nil,
		Credits:         s.elevenCredits(r.Context()),
		Tick:            s.tickView(r),
	})
}

func (s *server) renderAdminStrands(w http.ResponseWriter, r *http.Request, u store.User, msg string, status int) {
	canon, err := s.store.ListStrands(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	rows := make([]adminStrandRow, 0, len(canon))
	for _, st := range canon {
		airings, err := s.store.ListAirings(r.Context(), st.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		rows = append(rows, adminStrandRow{
			Strand:    st,
			Airings:   len(airings),
			Deletable: len(airings) == 0,
			ArtText:   firstNonEmpty(st.ArtText, st.Title),
			Accent:    st.Accent,
			Icon:      st.Icon,
			Uploaded:  st.CoverType != "" && st.ArtText == "",
		})
	}
	s.render(w, r, status, s.tmplAdminStrands, adminStrandsPage{
		User:     u,
		Strands:  rows,
		Error:    msg,
		Accents:  coverart.AccentNames(),
		Icons:    coverart.IconNames(),
		ReturnTo: r.URL.RequestURI(),
	})
}

// handleAdminStrandCreate adds one entry to the canon, and draws its cover
// art from the title on the way in. A Strand with no art is Dormant — a
// podcast feed with no <itunes:image> is broken in most clients — and
// asking an admin to open a design tool before the canon can grow is how a
// canon stops growing, so the art is generated (ADR 0020).
// A title the generator cannot set still creates the Strand; it just stays
// Dormant until somebody generates from shorter words or uploads a file.
func (s *server) handleAdminStrandCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	st := store.Strand{
		ID:          strings.TrimSpace(r.FormValue("id")),
		Title:       strings.TrimSpace(r.FormValue("title")),
		Description: strings.TrimSpace(r.FormValue("description")),
		CreatedAt:   time.Now().UTC(),
	}
	if err := store.ValidateStrand(st); err != nil {
		s.renderAdminStrands(w, r, u, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if _, err := s.store.GetStrand(r.Context(), st.ID); err == nil {
		s.renderAdminStrands(w, r, u, "there is already a strand with that id", http.StatusConflict)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	spec := specFrom(r, st.Title)
	rememberSpec(&st, spec)
	if err := s.store.PutStrand(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	if _, err := s.putGeneratedCover(r, st.ID, spec); err != nil {
		if errors.Is(err, errCoverArt) {
			s.renderAdminStrands(w, r, u,
				"the strand was added, but its art could not be drawn ("+
					strings.TrimPrefix(err.Error(), errCoverArt.Error()+": ")+
					"), so it is dormant — generate it from shorter words or upload a file",
				http.StatusOK)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/admin/strands"), http.StatusSeeOther)
}

// specFrom reads the Art Spec off a form. Empty words mean the title,
// which is what makes a Strand's art follow its name unless somebody
// says otherwise; empty accent and icon mean "derive from the words".
func specFrom(r *http.Request, title string) coverart.Spec {
	return coverart.Spec{
		Text:   firstNonEmpty(strings.TrimSpace(r.FormValue("art_text")), title),
		Accent: strings.TrimSpace(r.FormValue("accent")),
		Icon:   strings.TrimSpace(r.FormValue("icon")),
	}
}

// storedSpec is the Spec a Strand's current art was drawn from, or the
// zero Spec when the art was uploaded and there is nothing to compare.
func storedSpec(st store.Strand) coverart.Spec {
	return coverart.Spec{Text: st.ArtText, Accent: st.Accent, Icon: st.Icon}
}

// rememberSpec records how the art was just made, so the form can show
// the truth on reload instead of guessing from the title.
func rememberSpec(st *store.Strand, spec coverart.Spec) {
	st.ArtText, st.Accent, st.Icon = spec.Text, spec.Accent, spec.Icon
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// errCoverArt marks a failure that is the admin's wording, not the
// server's fault: too many words to set, an unknown accent. The caller
// turns it into a message on the page instead of a 500.
var errCoverArt = errors.New("cover art")

// putGeneratedCover draws the art and stores it as this Strand's cover,
// which is also what wakes a Dormant one up. It returns the stored cover
// type because SetStrandCover writes that field directly: a caller that
// goes on to PutStrand a Strand it read *before* this call would write
// the old empty type back and put the Strand straight back to sleep.
func (s *server) putGeneratedCover(r *http.Request, id string, spec coverart.Spec) (string, error) {
	p, err := coverart.Generate(spec)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errCoverArt, err)
	}
	err = s.store.SetStrandCover(r.Context(), id, p.FullType,
		bytes.NewReader(p.Full), bytes.NewReader(p.Thumb))
	if err != nil {
		return "", err
	}
	return p.FullType, nil
}

// handleAdminStrandGenerateCover redraws a Strand's art from words the
// admin can retype, with the accent and icon either chosen or derived.
func (s *server) handleAdminStrandGenerateCover(w http.ResponseWriter, r *http.Request, u store.User) {
	st, ok := s.adminStrandOr404(w, r)
	if !ok {
		return
	}
	spec := specFrom(r, st.Title)
	coverType, err := s.putGeneratedCover(r, st.ID, spec)
	if err != nil {
		if errors.Is(err, errCoverArt) {
			s.renderAdminStrands(w, r, u, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		s.fail(w, err)
		return
	}
	st.CoverType = coverType
	rememberSpec(&st, spec)
	if err := s.store.PutStrand(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/admin/strands"), http.StatusSeeOther)
}

// previewEdge is the size of the admin preview. Big enough to judge the
// type, small enough to redraw on every keystroke.
const previewEdge = 512

// handleAdminCoverPreview draws art without storing it, so the admin page
// can show what a title would turn into before committing to it.
func (s *server) handleAdminCoverPreview(w http.ResponseWriter, r *http.Request, _ store.User) {
	q := r.URL.Query()
	img, err := coverart.Render(coverart.Spec{
		Text:   strings.TrimSpace(q.Get("text")),
		Accent: strings.TrimSpace(q.Get("accent")),
		Icon:   strings.TrimSpace(q.Get("icon")),
	}, previewEdge)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Write(buf.Bytes())
}

// handleAdminStrandUpdate is the one Save on a Strand: its words and its
// art together, the way creating one already worked. The id is not among
// them — it addresses the public feed, and renaming it would silently
// kill every subscription.
//
// The art is redrawn only when the Art Spec actually changed (ADR 0021).
// That is what makes one button safe: fixing a typo in a description
// leaves the cover alone, and an uploaded cover — which has no Spec —
// is never overwritten by an edit to the wording.
func (s *server) handleAdminStrandUpdate(w http.ResponseWriter, r *http.Request, u store.User) {
	st, ok := s.adminStrandOr404(w, r)
	if !ok {
		return
	}
	before := storedSpec(st)
	st.Title = strings.TrimSpace(r.FormValue("title"))
	st.Description = strings.TrimSpace(r.FormValue("description"))
	if err := store.ValidateStrand(st); err != nil {
		s.renderAdminStrands(w, r, u, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	// An uploaded cover has no Spec, and the form's art fields are shown
	// empty against it. Leaving them alone must mean "keep my upload",
	// not "draw the title over it".
	uploaded := st.CoverType != "" && before.Text == ""
	after := specFrom(r, st.Title)
	if uploaded && strings.TrimSpace(r.FormValue("art_text")) == "" {
		after = before
	}

	if after != before {
		coverType, err := s.putGeneratedCover(r, st.ID, after)
		if err != nil {
			if errors.Is(err, errCoverArt) {
				s.renderAdminStrands(w, r, u, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			s.fail(w, err)
			return
		}
		st.CoverType = coverType
		rememberSpec(&st, after)
	}
	if err := s.store.PutStrand(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/admin/strands"), http.StatusSeeOther)
}

// handleAdminStrandCover uploads the art, which is also what wakes a
// Dormant Strand up. Same two derivatives a feed's Cover Art gets, from
// the same package.
func (s *server) handleAdminStrandCover(w http.ResponseWriter, r *http.Request, u store.User) {
	st, ok := s.adminStrandOr404(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		s.renderAdminStrands(w, r, u, "could not read the upload", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("cover")
	if err != nil {
		s.renderAdminStrands(w, r, u, "choose an image first", http.StatusBadRequest)
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	if contentType != "image/jpeg" && contentType != "image/png" {
		s.renderAdminStrands(w, r, u, "cover art must be a JPEG or a PNG", http.StatusUnsupportedMediaType)
		return
	}
	p, err := coverart.Process(http.MaxBytesReader(w, file, 8<<20), contentType)
	if err != nil {
		s.renderAdminStrands(w, r, u, "could not process that image: "+err.Error(), http.StatusBadRequest)
		return
	}
	err = s.store.SetStrandCover(r.Context(), st.ID, p.FullType,
		bytes.NewReader(p.Full), bytes.NewReader(p.Thumb))
	if err != nil {
		s.fail(w, err)
		return
	}
	// This art did not come from words, so it has no Spec. Clearing it
	// is what lets everything downstream know the cover was uploaded —
	// and what stops a later Save from redrawing over it (ADR 0021).
	// The cover type comes along for the same reason it does above.
	st.CoverType = p.FullType
	rememberSpec(&st, coverart.Spec{})
	if err := s.store.PutStrand(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/admin/strands"), http.StatusSeeOther)
}

// handleAdminStrandAction dispatches the three verbs that change a
// Strand's standing rather than its words.
func (s *server) handleAdminStrandAction(w http.ResponseWriter, r *http.Request, u store.User) {
	switch r.PathValue("action") {
	case "retire":
		s.setStrandRetired(w, r, true)
	case "unretire":
		s.setStrandRetired(w, r, false)
	case "delete":
		s.handleAdminStrandDelete(w, r, u)
	default:
		s.renderNotFound(w, r)
	}
}

// setStrandRetired takes a Strand out of the canon without taking it off
// the internet: no new Airings, no place in discovery, but its page and
// feed keep serving whoever already subscribed (ADR 0017).
func (s *server) setStrandRetired(w http.ResponseWriter, r *http.Request, retired bool) {
	st, ok := s.adminStrandOr404(w, r)
	if !ok {
		return
	}
	st.Retired = retired
	if err := s.store.PutStrand(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/admin/strands"), http.StatusSeeOther)
}

// handleAdminStrandDelete removes an entry outright, and only when
// nothing has ever aired on it. Anything else retires: deleting a
// Strand with Airings would reach into other people's publishing
// decisions and break links they have already handed out.
func (s *server) handleAdminStrandDelete(w http.ResponseWriter, r *http.Request, u store.User) {
	st, ok := s.adminStrandOr404(w, r)
	if !ok {
		return
	}
	airings, err := s.store.ListAirings(r.Context(), st.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(airings) > 0 {
		s.renderAdminStrands(w, r, u,
			"episodes have aired on that strand, so it can only be retired", http.StatusConflict)
		return
	}
	if err := s.store.DeleteStrand(r.Context(), st.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, returnTo(r, "/admin/strands"), http.StatusSeeOther)
}

func (s *server) adminStrandOr404(w http.ResponseWriter, r *http.Request) (store.Strand, bool) {
	st, err := s.store.GetStrand(r.Context(), r.PathValue("strand"))
	if err != nil {
		s.fail(w, err)
		return store.Strand{}, false
	}
	return st, true
}
