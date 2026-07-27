// Package feed renders a User's Personal Feed as podcast RSS (RSS 2.0
// with the iTunes namespace tags podcast clients expect).
package feed

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"time"

	"github.com/nicocesar/podcasting_server/internal/store"
)

type rss struct {
	XMLName  xml.Name `xml:"rss"`
	Version  string   `xml:"version,attr"`
	ItunesNS string   `xml:"xmlns:itunes,attr"`
	Channel  channel  `xml:"channel"`
}

type channel struct {
	Title         string `xml:"title"`
	Link          string `xml:"link"`
	Description   string `xml:"description"`
	Language      string `xml:"language,omitempty"`
	LastBuildDate string `xml:"lastBuildDate,omitempty"`
	// A private feed: ask directories not to index it should the URL leak.
	ItunesBlock string       `xml:"itunes:block"`
	Image       *itunesImage `xml:"itunes:image,omitempty"`
	Items       []item       `xml:"item"`
}

type itunesImage struct {
	Href string `xml:"href,attr"`
}

type item struct {
	Title       string `xml:"title"`
	GUID        guid   `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description,omitempty"`
	// The Owner's ID, so a mixed feed shows where each episode came from.
	Author    string    `xml:"itunes:author"`
	Duration  string    `xml:"itunes:duration,omitempty"`
	Enclosure enclosure `xml:"enclosure"`
}

type guid struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

// Item is one entry in a rendered feed: the Episode, the address its
// audio is fetched from, and who it is credited to. The enclosure URL
// is passed in rather than derived because it is not always the same
// namespace — an Episode delivered by a Follow is public, and points at
// its Strand rather than into the reader's Feed Token (ADR 0019).
type Item struct {
	Episode store.Episode
	// EnclosureURL is the absolute address of the audio.
	EnclosureURL string
	// Author is the item's itunes:author.
	Author string
}

// buildItem renders one entry. The GUID derives from (owner, slug) in
// every feed, so an Episode that is aired, shared, and delivered is one
// item to a podcast client rather than three (ADR 0002/0006/0008).
func buildItem(in Item) item {
	ep := in.Episode
	it := item{
		Title:       ep.Title,
		GUID:        guid{IsPermaLink: "false", Value: ep.OwnerID + "/" + ep.Slug},
		PubDate:     ep.PublishedAt.UTC().Format(time.RFC1123Z),
		Description: ep.Description,
		Author:      in.Author,
		Enclosure: enclosure{
			URL:    in.EnclosureURL,
			Length: ep.AudioSize,
			Type:   ep.AudioType,
		},
	}
	if ep.DurationSec > 0 {
		it.Duration = strconv.Itoa(ep.DurationSec)
	}
	return it
}

// render marshals a finished channel with the XML header podcast
// clients expect.
func render(ch channel) ([]byte, error) {
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

// RSS renders u's Personal Feed: their own Episodes, those shared into
// the feed, and those a Follow delivers — each carrying its Owner, and
// already sorted newest-first. baseURL is the server's external base URL
// without a trailing slash.
func RSS(u store.User, items []Item, baseURL string) ([]byte, error) {
	feedBase := fmt.Sprintf("%s/f/%s", baseURL, u.FeedToken)
	ch := channel{
		Title:       u.Title,
		Link:        feedBase,
		Description: u.Description,
		Language:    u.Language,
		ItunesBlock: "Yes",
	}
	if u.CoverType != "" {
		ch.Image = &itunesImage{Href: feedBase + "/cover"}
	}
	if len(items) > 0 {
		ch.LastBuildDate = items[0].Episode.PublishedAt.UTC().Format(time.RFC1123Z)
	}
	for _, in := range items {
		ch.Items = append(ch.Items, buildItem(in))
	}
	return render(ch)
}

// FeedTokenEnclosure is the address of an Episode's audio inside a
// reader's own capability namespace — the default for anything that is
// theirs or was shared to them (ADR 0008).
func FeedTokenEnclosure(baseURL, feedToken string, ep store.Episode) string {
	return fmt.Sprintf("%s/f/%s/%s/%s.mp3", baseURL, feedToken, ep.OwnerID, ep.Slug)
}
