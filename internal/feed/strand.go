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
	"encoding/xml"
	"fmt"
	"strconv"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

// StrandItem is one Aired Episode as its Strand Feed sees it: the
// Episode itself, the public id that addresses its audio, and the
// Owner's feed title, which is how an Aired Episode is attributed
// (never by username — ADR 0018).
type StrandItem struct {
	AiringID string
	Episode  store.Episode
	Author   string
}

// StrandRSS renders one Strand as a public podcast feed. items must
// already be sorted newest-first and capped by the caller. baseURL is
// the server's external base URL without a trailing slash.
func StrandRSS(st store.Strand, items []StrandItem, baseURL string) ([]byte, error) {
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
		ep := in.Episode
		it := item{
			Title:       ep.Title,
			GUID:        guid{IsPermaLink: "false", Value: ep.OwnerID + "/" + ep.Slug},
			PubDate:     ep.PublishedAt.UTC().Format(time.RFC1123Z),
			Description: ep.Description,
			// Attribution is the Owner's feed title, so the username —
			// which is also the Share address — stays private.
			Author: in.Author,
			Enclosure: enclosure{
				URL:    fmt.Sprintf("%s/%s.mp3", strandBase, in.AiringID),
				Length: ep.AudioSize,
				Type:   ep.AudioType,
			},
		}
		if ep.DurationSec > 0 {
			it.Duration = strconv.Itoa(ep.DurationSec)
		}
		ch.Items = append(ch.Items, it)
	}
	body, err := xml.MarshalIndent(rss{
		Version:  "2.0",
		ItunesNS: "http://www.itunes.com/dtds/podcast-1.0.dtd",
		Channel:  ch,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}
