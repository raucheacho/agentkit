package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/raucheacho/agentkit/observe"
)

// rssPoller relève des flux RSS ou Atom : chaque article devient un
// événement, identifié par son lien (les déjà vus sont écartés par observe).
func rssPoller(urls []string) observe.Poller {
	client := &http.Client{Timeout: 30 * time.Second}
	return observe.PollerFunc("rss", func(ctx context.Context) ([]observe.Event, error) {
		var events []observe.Event
		for _, u := range urls {
			items, err := fetchFeed(ctx, client, u)
			if err != nil {
				return events, fmt.Errorf("%s : %w", u, err)
			}
			for _, it := range items {
				events = append(events, observe.Event{Key: "rss:" + it.id, Source: "rss", Title: it.title, Body: it.summary + "\n" + it.link})
			}
		}
		return events, nil
	})
}

type item struct{ id, title, link, summary string }

func fetchFeed(ctx context.Context, client *http.Client, url string) ([]item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var feed struct {
		Channel struct {
			Items []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				GUID        string `xml:"guid"`
				Description string `xml:"description"`
			} `xml:"item"`
		} `xml:"channel"`
		Entries []struct {
			ID      string `xml:"id"`
			Title   string `xml:"title"`
			Summary string `xml:"summary"`
			Link    struct {
				Href string `xml:"href,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&feed); err != nil {
		return nil, err
	}
	var out []item
	for _, it := range feed.Channel.Items {
		id := it.GUID
		if id == "" {
			id = it.Link
		}
		out = append(out, item{id: id, title: it.Title, link: it.Link, summary: it.Description})
	}
	for _, e := range feed.Entries {
		out = append(out, item{id: e.ID, title: e.Title, link: e.Link.Href, summary: e.Summary})
	}
	// Les 20 plus récents suffisent : les autres ont été vus aux relèves précédentes.
	if len(out) > 20 {
		out = out[:20]
	}
	return out, nil
}
