package feed

// A Strand Feed: the same RSS a Personal Feed renders as, but public and
// multi-author (ADR 0018). Two things differ and both matter.
//
// Enclosures live at /strands/{strand}/{airing}.mp3 rather than inside a
// Feed Token namespace, because there is no capability here — the id is
// the whole address, and it is opaque so the Owner's username stays out
// of public URLs. The GUID still derives from (owner, slug), so an
// Episode that is both aired and in someone's Personal Feed remains one
// item to a podcast client rather than two.
//
// itunes:block stays set. A Strand Feed is reachable by URL and listed
// in no directory: one attribute to flip once there is a moderation
// story worth standing behind.

import (
	"fmt"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// StrandEnclosure is the address of an Aired Episode's audio on the
// public side: the opaque Airing id and nothing else, so the Owner's
// username stays out of public URLs (ADR 0018).
func StrandEnclosure(baseURL, strandID, airingID string) string {
	return fmt.Sprintf("%s/strands/%s/%s.mp3", baseURL, strandID, airingID)
}

// StrandRSS renders one Strand as a public podcast feed. items must
// already be sorted newest-first and capped by the caller. baseURL is
// the server's external base URL without a trailing slash.
func StrandRSS(st store.Strand, items []Item, baseURL string) ([]byte, error) {
	strandBase := fmt.Sprintf("%s/strands/%s", baseURL, st.ID)
	ch := channel{
		Title:       st.Title,
		Link:        strandBase,
		Description: st.Description,
		ItunesBlock: "Yes",
	}
	// A Strand cannot be aired into before it has art, so this is set in
	// practice; the check keeps a half-built canon from rendering an
	// <itunes:image> with nothing behind it.
	if st.CoverType != "" {
		ch.Image = &itunesImage{Href: strandBase + "/cover"}
	}
	if len(items) > 0 {
		ch.LastBuildDate = items[0].Episode.PublishedAt.UTC().Format(time.RFC1123Z)
	}
	for _, in := range items {
		ch.Items = append(ch.Items, buildItem(in))
	}
	return render(ch)
}
