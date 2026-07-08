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

// RSS renders u's Personal Feed. episodes mixes the user's own episodes
// with those shared into the feed — each carries its Owner — and must
// already be sorted newest-first. baseURL is the server's external base
// URL without a trailing slash.
func RSS(u store.User, episodes []store.Episode, baseURL string) ([]byte, error) {
	ch := channel{
		Title:       u.Title,
		Link:        fmt.Sprintf("%s/users/%s", baseURL, u.ID),
		Description: u.Description,
		Language:    u.Language,
		ItunesBlock: "Yes",
	}
	if u.CoverType != "" && u.CoverSecret != "" {
		ch.Image = &itunesImage{Href: fmt.Sprintf("%s/covers/%s", baseURL, u.CoverSecret)}
	}
	if len(episodes) > 0 {
		ch.LastBuildDate = episodes[0].PublishedAt.UTC().Format(time.RFC1123Z)
	}
	for _, ep := range episodes {
		it := item{
			Title: ep.Title,
			// GUID derives from (owner, slug): a replaced episode keeps its
			// GUID everywhere, and one episode shared into many feeds is the
			// same item in each (ADR 0002/0006).
			GUID:        guid{IsPermaLink: "false", Value: ep.OwnerID + "/" + ep.Slug},
			PubDate:     ep.PublishedAt.UTC().Format(time.RFC1123Z),
			Description: ep.Description,
			Author:      ep.OwnerID,
			Enclosure: enclosure{
				// Canonical, owner-addressed URL: the same enclosure in every
				// feed the episode is shared into.
				URL:    fmt.Sprintf("%s/users/%s/episodes/%s.mp3", baseURL, ep.OwnerID, ep.Slug),
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
