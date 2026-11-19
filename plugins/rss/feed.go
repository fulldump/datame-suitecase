package rss

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// The three shapes of feeds in the wild: RSS 2.0 (<rss><channel><item>), RSS
// 1.0 (<rdf:RDF><item>) and Atom (<feed><entry>). One struct reads them all,
// matching by local name.

type document struct {
	XMLName xml.Name
	Channel struct {
		Title string `xml:"title"`
		Items []item `xml:"item"`
	} `xml:"channel"`
	Items   []item  `xml:"item"`
	Title   string  `xml:"title"`
	Entries []entry `xml:"entry"`
}

type item struct {
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	GUID        string   `xml:"guid"`
	PubDate     string   `xml:"pubDate"`
	Date        string   `xml:"http://purl.org/dc/elements/1.1/ date"`
	Description string   `xml:"description"`
	Content     string   `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	Author      string   `xml:"author"`
	Creator     string   `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Categories  []string `xml:"category"`
	Enclosures  []struct {
		URL    string `xml:"url,attr"`
		Type   string `xml:"type,attr"`
		Length string `xml:"length,attr"`
	} `xml:"enclosure"`
}

type atomText struct {
	Type  string `xml:"type,attr"`
	Text  string `xml:",chardata"`
	Inner string `xml:",innerxml"`
}

// html returns the text as HTML: xhtml comes as markup, html escaped, text plain.
func (t atomText) html() string {
	switch t.Type {
	case "xhtml":
		return strings.TrimSpace(t.Inner)
	case "html", "text/html":
		return strings.TrimSpace(t.Text)
	}
	if strings.TrimSpace(t.Text) == "" {
		return ""
	}
	return "<p>" + xmlEscape(strings.TrimSpace(t.Text)) + "</p>"
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

type entry struct {
	Title atomText `xml:"title"`
	ID    string   `xml:"id"`
	Links []struct {
		Href string `xml:"href,attr"`
		Rel  string `xml:"rel,attr"`
	} `xml:"link"`
	Published string   `xml:"published"`
	Updated   string   `xml:"updated"`
	Summary   atomText `xml:"summary"`
	Content   atomText `xml:"content"`
	Author    struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Categories []struct {
		Term string `xml:"term,attr"`
	} `xml:"category"`
}

var dateLayouts = []string{
	time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC3339Nano,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", "2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04 -0700", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func (d *document) entries(feedURL string) []Entry {
	feed := FeedInfo{Title: strings.TrimSpace(d.Channel.Title), URL: feedURL}
	if feed.Title == "" {
		feed.Title = strings.TrimSpace(d.Title)
	}
	out := []Entry{}
	for _, it := range append(d.Channel.Items, d.Items...) {
		e := Entry{Title: strings.TrimSpace(it.Title), Link: strings.TrimSpace(it.Link), ID: strings.TrimSpace(it.GUID),
			Author: strings.TrimSpace(it.Creator), Summary: strings.TrimSpace(it.Description), Content: strings.TrimSpace(it.Content), Feed: feed}
		if e.Author == "" {
			e.Author = strings.TrimSpace(it.Author)
		}
		for _, c := range it.Categories {
			if c = strings.TrimSpace(c); c != "" {
				e.Categories = append(e.Categories, c)
			}
		}
		for _, enc := range it.Enclosures {
			e.Enclosures = append(e.Enclosures, Enclosure{URL: enc.URL, Type: enc.Type, Length: enc.Length})
		}
		date := it.PubDate
		if date == "" {
			date = it.Date
		}
		e.Published, e.dated = parseDate(date)
		out = append(out, e)
	}
	for _, en := range d.Entries {
		e := Entry{Title: strings.TrimSpace(en.Title.Text), ID: strings.TrimSpace(en.ID), Author: strings.TrimSpace(en.Author.Name),
			Summary: en.Summary.html(), Content: en.Content.html(), Feed: feed}
		if en.Title.Type == "xhtml" {
			e.Title = strings.TrimSpace(en.Title.Inner)
		}
		for _, link := range en.Links {
			if link.Rel == "" || link.Rel == "alternate" {
				e.Link = link.Href
				break
			}
		}
		for _, c := range en.Categories {
			e.Categories = append(e.Categories, c.Term)
		}
		published := en.Published
		if published == "" {
			published = en.Updated
		}
		e.Published, e.dated = parseDate(published)
		if updated, ok := parseDate(en.Updated); ok && !updated.Equal(e.Published) {
			e.Updated = updated
		}
		out = append(out, e)
	}
	for i := range out {
		if out[i].ID == "" {
			out[i].ID = out[i].Link
		}
		if out[i].ID == "" {
			out[i].ID = out[i].Title + "|" + out[i].Published.String()
		}
		if out[i].Title == "" {
			out[i].Title = "(sin título)"
		}
	}
	return out
}

// fetch downloads the feed. It returns nil without error when the server says
// it has not changed since the last time.
func fetch(ctx context.Context, client *http.Client, feedURL string, c cursor) (*document, cursor, error) {
	if !strings.HasPrefix(feedURL, "http://") && !strings.HasPrefix(feedURL, "https://") {
		return nil, c, errors.New("la dirección del feed debe empezar por http:// o https://")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", feedURL, nil)
	if err != nil {
		return nil, c, err
	}
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.5")
	if c.ETag != "" {
		req.Header.Set("If-None-Match", c.ETag)
	}
	if c.LastModified != "" {
		req.Header.Set("If-Modified-Since", c.LastModified)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, c, fmt.Errorf("no se pudo descargar el feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, c, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c, fmt.Errorf("el feed respondió %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFeed))
	if err != nil {
		return nil, c, err
	}
	doc, err := parse(data)
	if err != nil {
		return nil, c, err
	}
	next := c
	next.ETag, next.LastModified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	return doc, next, nil
}

func parse(data []byte) (*document, error) {
	var doc document
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = false
	decoder.Entity = xml.HTMLEntity
	decoder.CharsetReader = charsetReader
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("no parece un feed RSS o Atom: %w", err)
	}
	switch strings.ToLower(doc.XMLName.Local) {
	case "rss", "rdf", "feed":
		return &doc, nil
	}
	return nil, errors.New("no parece un feed RSS o Atom: es una página u otro documento")
}

// charsetReader handles the old Latin-1 feeds; everything else must be UTF-8.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "iso-8859-15", "latin1", "windows-1252", "cp1252":
		data, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, len(data)+len(data)/8)
		for _, b := range data {
			out = utf8.AppendRune(out, rune(b))
		}
		return bytes.NewReader(out), nil
	}
	return nil, fmt.Errorf("codificación %q no soportada", charset)
}
