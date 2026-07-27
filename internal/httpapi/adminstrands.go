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
	// ArtText is what generated art would say — the title, lowercased by
	// the generator. Kept separate because the words on the art and the
	// words in the feed do not have to match.
	ArtText string
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
// nothing and decides nothing — every surface it names guards itself.
func (s *server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, s.tmplAdmin, struct {
		CostsConfigured bool
	}{CostsConfigured: s.adminAPI != nil})
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
			ArtText:   st.Title,
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
	if err := s.store.PutStrand(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	spec := coverart.Spec{
		Text:   firstNonEmpty(strings.TrimSpace(r.FormValue("art_text")), st.Title),
		Accent: strings.TrimSpace(r.FormValue("accent")),
		Icon:   strings.TrimSpace(r.FormValue("icon")),
	}
	if err := s.putGeneratedCover(r, st.ID, spec); err != nil {
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
// which is also what wakes a Dormant one up.
func (s *server) putGeneratedCover(r *http.Request, id string, spec coverart.Spec) error {
	p, err := coverart.Generate(spec)
	if err != nil {
		return fmt.Errorf("%w: %w", errCoverArt, err)
	}
	return s.store.SetStrandCover(r.Context(), id, p.FullType,
		bytes.NewReader(p.Full), bytes.NewReader(p.Thumb))
}

// handleAdminStrandGenerateCover redraws a Strand's art from words the
// admin can retype, with the accent and icon either chosen or derived.
func (s *server) handleAdminStrandGenerateCover(w http.ResponseWriter, r *http.Request, u store.User) {
	st, ok := s.adminStrandOr404(w, r)
	if !ok {
		return
	}
	spec := coverart.Spec{
		Text:   firstNonEmpty(strings.TrimSpace(r.FormValue("art_text")), st.Title),
		Accent: strings.TrimSpace(r.FormValue("accent")),
		Icon:   strings.TrimSpace(r.FormValue("icon")),
	}
	if err := s.putGeneratedCover(r, st.ID, spec); err != nil {
		if errors.Is(err, errCoverArt) {
			s.renderAdminStrands(w, r, u, err.Error(), http.StatusUnprocessableEntity)
			return
		}
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

// handleAdminStrandUpdate edits the words. The id is not among them.
func (s *server) handleAdminStrandUpdate(w http.ResponseWriter, r *http.Request, u store.User) {
	st, ok := s.adminStrandOr404(w, r)
	if !ok {
		return
	}
	st.Title = strings.TrimSpace(r.FormValue("title"))
	st.Description = strings.TrimSpace(r.FormValue("description"))
	if err := store.ValidateStrand(st); err != nil {
		s.renderAdminStrands(w, r, u, err.Error(), http.StatusUnprocessableEntity)
		return
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
