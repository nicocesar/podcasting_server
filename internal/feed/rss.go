// Package feed renders a Show and its Episodes as podcast RSS (RSS 2.0
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
	Title         string       `xml:"title"`
	Link          string       `xml:"link"`
	Description   string       `xml:"description"`
	Language      string       `xml:"language,omitempty"`
	LastBuildDate string       `xml:"lastBuildDate,omitempty"`
	// A private feed: ask directories not to index it should the URL leak.
	ItunesBlock string       `xml:"itunes:block"`
	Image       *itunesImage `xml:"itunes:image,omitempty"`
	Items       []item       `xml:"item"`
}

type itunesImage struct {
	Href string `xml:"href,attr"`
}

type item struct {
	Title       string    `xml:"title"`
	GUID        guid      `xml:"guid"`
	PubDate     string    `xml:"pubDate"`
	Description string    `xml:"description,omitempty"`
	Duration    string    `xml:"itunes:duration,omitempty"`
	Enclosure   enclosure `xml:"enclosure"`
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

// RSS renders the feed. baseURL is the server's external base URL without
// a trailing slash; episodes must already be sorted newest-first.
func RSS(show store.Show, episodes []store.Episode, baseURL string) ([]byte, error) {
	ch := channel{
		Title:       show.Title,
		Link:        fmt.Sprintf("%s/shows/%s", baseURL, show.ID),
		Description: show.Description,
		Language:    show.Language,
		ItunesBlock: "Yes",
	}
	if show.CoverType != "" {
		ch.Image = &itunesImage{Href: fmt.Sprintf("%s/shows/%s/cover", baseURL, show.ID)}
	}
	if len(episodes) > 0 {
		ch.LastBuildDate = episodes[0].PublishedAt.UTC().Format(time.RFC1123Z)
	}
	for _, ep := range episodes {
		it := item{
			Title: ep.Title,
			// GUID derives from (show, slug): a replaced episode keeps its
			// GUID, so clients treat it as the same item (ADR 0002).
			GUID:        guid{IsPermaLink: "false", Value: show.ID + "/" + ep.Slug},
			PubDate:     ep.PublishedAt.UTC().Format(time.RFC1123Z),
			Description: ep.Description,
			Enclosure: enclosure{
				URL:    fmt.Sprintf("%s/shows/%s/episodes/%s.mp3", baseURL, show.ID, ep.Slug),
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
