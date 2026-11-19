// Package rss keeps a copy of a blog through its RSS or Atom feed: each post as
// JSON, Markdown and a standalone HTML page, with its images beside it.
package rss

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

const (
	maxFeed  = 20 << 20
	maxImage = 30 << 20
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the RSS/Atom integration.
type Plugin struct{}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:              "rss",
		Title:             "Blog (RSS o Atom)",
		Description:       "Guarda cada entrada de un blog con sus imágenes: Markdown, una página HTML y JSON.",
		Help:              "Pega la dirección del feed del blog. Suele estar en /feed, /rss.xml o /atom.xml (WordPress: https://tublog.com/feed). Los feeds solo traen las últimas entradas, así que conviene darlo de alta cuanto antes: desde entonces no se perderá ninguna.",
		DefaultFolder:     "/Archivo/Blog",
		DefaultEveryHours: 6,
		Fields: []plugin.Field{
			{Name: "url", Label: "Dirección del feed", Type: "url", Required: true, Placeholder: "https://tublog.com/feed"},
			{Name: "images", Label: "Descargar las imágenes de las entradas", Type: "checkbox", Default: "true"},
		},
	}
}

// Validate implements plugin.Plugin: the feed must answer and be a feed.
func (p Plugin) Validate(ctx context.Context, env plugin.Env) error {
	_, _, err := fetch(ctx, env.HTTP, env.Config.Get("url"), cursor{})
	return err
}

type cursor struct {
	ETag         string            `json:"etag,omitempty"`
	LastModified string            `json:"last_modified,omitempty"`
	Seen         map[string]string `json:"seen,omitempty"`       // entry id → hash of what was written
	Names        map[string]string `json:"names,omitempty"`      // entry id → folder, so it never moves
	FirstSeen    map[string]int64  `json:"first_seen,omitempty"` // for entries without a date
	Images       map[string]string `json:"images,omitempty"`     // image URL → file, downloaded already
}

// Entry is a post, as saved in index.json.
type Entry struct {
	ID         string            `json:"id"`
	Title      string            `json:"title"`
	Link       string            `json:"link,omitempty"`
	Author     string            `json:"author,omitempty"`
	Published  time.Time         `json:"published"`
	Updated    time.Time         `json:"updated,omitzero"`
	Categories []string          `json:"categories,omitempty"`
	Summary    string            `json:"summary_html,omitempty"`
	Content    string            `json:"content_html,omitempty"`
	Enclosures []Enclosure       `json:"enclosures,omitempty"`
	Images     map[string]string `json:"images,omitempty"` // original URL → file beside index.json
	Feed       FeedInfo          `json:"feed"`
	dated      bool
}

// Enclosure is an attached file (a podcast episode), kept as a link.
type Enclosure struct {
	URL    string `json:"url"`
	Type   string `json:"type,omitempty"`
	Length string `json:"length,omitempty"`
}

// FeedInfo says where an entry came from.
type FeedInfo struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url"`
}

// Sync implements plugin.Plugin.
func (p Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	var c cursor
	if err := run.Load(&c); err != nil {
		return err
	}
	if c.Seen == nil {
		c.Seen, c.Names, c.FirstSeen, c.Images = map[string]string{}, map[string]string{}, map[string]int64{}, map[string]string{}
	}
	feedURL := run.Config.Get("url")
	feed, next, err := fetch(ctx, run.HTTP, feedURL, c)
	if err != nil {
		return err
	}
	if feed == nil {
		run.Logf("El feed no ha cambiado")
		return nil
	}
	entries := feed.entries(feedURL)
	run.Logf("El feed trae %d entradas", len(entries))
	used := map[string]bool{}
	for _, name := range c.Names {
		used[name] = true
	}
	images := run.Config.Get("images") != "false"

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.dated {
			if c.FirstSeen[entry.ID] == 0 {
				c.FirstSeen[entry.ID] = run.Now().Unix()
			}
			entry.Published = time.Unix(c.FirstSeen[entry.ID], 0).UTC()
		}
		fingerprint := entryHash(entry)
		if c.Seen[entry.ID] == fingerprint {
			continue
		}
		folder := c.Names[entry.ID]
		if folder == "" {
			folder = folderFor(entry, used)
			c.Names[entry.ID] = folder
			used[folder] = true
		}
		if err := writeEntry(ctx, run, &c, folder, entry, images); err != nil {
			return err
		}
		if _, existed := c.Seen[entry.ID]; !existed {
			run.Item(1)
		}
		c.Seen[entry.ID] = fingerprint
		if err := run.Save(c); err != nil {
			return err
		}
	}
	// The validators go last: if the run is cut, the next one fetches again.
	c.ETag, c.LastModified = next.ETag, next.LastModified
	return run.Save(c)
}

func entryHash(e Entry) string {
	sum := sha256.Sum256(plugin.JSON(e))
	return hex.EncodeToString(sum[:8])
}

// folderFor names the folder of a post by its date and the last part of its
// link, which rarely changes, or its title.
func folderFor(e Entry, used map[string]bool) string {
	slug := ""
	if u, err := url.Parse(e.Link); err == nil {
		slug = plugin.Slug(strings.TrimSuffix(path.Base(strings.TrimRight(u.Path, "/")), path.Ext(u.Path)), 60)
	}
	if slug == "" || slug == "index" {
		slug = plugin.Slug(e.Title, 60)
	}
	if slug == "" {
		slug = plugin.ShortHash(e.ID)
	}
	day := e.Published.UTC()
	name := fmt.Sprintf("%04d/%s-%s", day.Year(), day.Format("2006-01-02"), slug)
	if used[name] {
		name += "-" + plugin.ShortHash(e.ID)[:6]
	}
	return name
}

var imgSrc = regexp.MustCompile(`(?i)<img\b[^>]*?\bsrc\s*=\s*("([^"]*)"|'([^']*)')`)

func writeEntry(ctx context.Context, run *plugin.Run, c *cursor, folder string, entry Entry, images bool) error {
	body := entry.Content
	if body == "" {
		body = entry.Summary
	}
	base, _ := url.Parse(entry.Link)
	if base == nil || base.Host == "" {
		base, _ = url.Parse(entry.Feed.URL)
	}
	entry.Images = map[string]string{}
	if images {
		for _, match := range imgSrc.FindAllStringSubmatch(body, 50) {
			raw := html.UnescapeString(match[2] + match[3])
			ref, err := url.Parse(raw)
			if err != nil {
				continue
			}
			abs := base.ResolveReference(ref)
			if abs.Scheme != "http" && abs.Scheme != "https" {
				continue
			}
			file, err := saveImage(ctx, run, c, folder, abs.String())
			if err != nil {
				run.Logf("No se pudo guardar la imagen %s: %s", abs, err)
				continue
			}
			entry.Images[raw] = file
		}
	}

	modTime := entry.Published
	if !entry.Updated.IsZero() {
		modTime = entry.Updated
	}
	files := []plugin.File{
		{Path: folder + "/index.json", Data: plugin.JSON(entry)},
		{Path: folder + "/index.md", Data: markdown(entry, body)},
		{Path: folder + "/index.html", Data: page(entry, body)},
	}
	for _, f := range files {
		f.ModTime, f.Mutable = modTime, true
		if _, err := run.Out.Write(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

// saveImage downloads an image once and writes it in the media folder of the post.
func saveImage(ctx context.Context, run *plugin.Run, c *cursor, folder, src string) (string, error) {
	if file, ok := c.Images[src]; ok && strings.HasPrefix(file, folder+"/") {
		return strings.TrimPrefix(file, folder+"/"), nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", src, nil)
	if err != nil {
		return "", err
	}
	resp, err := run.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("respondió %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImage+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxImage {
		return "", errors.New("es demasiado grande")
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.HasPrefix(contentType, "image/") {
		return "", fmt.Errorf("no es una imagen (%s)", contentType)
	}
	u, _ := url.Parse(src)
	name := plugin.SafeName(path.Base(u.Path), 60)
	if path.Ext(name) == "" {
		if exts, _ := mime.ExtensionsByType(contentType); len(exts) > 0 {
			name += exts[0]
		}
	}
	// The hash of the URL keeps two «image.jpg» of one post apart.
	rel := "media/" + plugin.ShortHash(src)[:8] + "-" + name
	modTime := run.Now()
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		modTime = t
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: folder + "/" + rel, ModTime: modTime, Data: data}); err != nil {
		return "", err
	}
	c.Images[src] = folder + "/" + rel
	return rel, nil
}

func rewriteImages(body string, images map[string]string) string {
	return imgSrc.ReplaceAllStringFunc(body, func(tag string) string {
		m := imgSrc.FindStringSubmatch(tag)
		raw := html.UnescapeString(m[2] + m[3])
		if local, ok := images[raw]; ok {
			return strings.Replace(tag, m[1], `"`+html.EscapeString(local)+`"`, 1)
		}
		return tag
	})
}

func markdown(e Entry, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n", oneLine(e.Title))
	fmt.Fprintf(&b, "- Fecha: %s\n", e.Published.Format("2006-01-02 15:04"))
	if e.Author != "" {
		fmt.Fprintf(&b, "- Autor: %s\n", oneLine(e.Author))
	}
	if e.Link != "" {
		fmt.Fprintf(&b, "- Original: <%s>\n", e.Link)
	}
	if len(e.Categories) > 0 {
		fmt.Fprintf(&b, "- Etiquetas: %s\n", strings.Join(e.Categories, ", "))
	}
	b.WriteString("\n")
	b.WriteString(plugin.HTMLToText(body))
	b.WriteString("\n")
	if len(e.Images) > 0 {
		b.WriteString("\n## Imágenes\n\n")
		for _, local := range sortedValues(e.Images) {
			fmt.Fprintf(&b, "![](%s)\n", local)
		}
	}
	for _, enc := range e.Enclosures {
		fmt.Fprintf(&b, "\nAdjunto: <%s>\n", enc.URL)
	}
	return b.Bytes()
}

func page(e Entry, body string) []byte {
	var b bytes.Buffer
	b.WriteString("<!doctype html>\n<html><head><meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	fmt.Fprintf(&b, "<title>%s</title>\n", html.EscapeString(e.Title))
	b.WriteString("<style>body{max-width:42em;margin:2em auto;padding:0 1em;font:17px/1.6 Georgia,serif;color:#1f2421}img{max-width:100%;height:auto}.meta{color:#5e6660;font:14px system-ui,sans-serif}</style>\n</head><body>\n")
	fmt.Fprintf(&b, "<h1>%s</h1>\n<p class=\"meta\">%s", html.EscapeString(e.Title), e.Published.Format("2006-01-02"))
	if e.Author != "" {
		fmt.Fprintf(&b, " · %s", html.EscapeString(e.Author))
	}
	if e.Link != "" {
		fmt.Fprintf(&b, " · <a href=\"%s\">original</a>", html.EscapeString(e.Link))
	}
	b.WriteString("</p>\n<article>\n")
	b.WriteString(rewriteImages(body, e.Images))
	b.WriteString("\n</article>\n</body></html>\n")
	return b.Bytes()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	seen := map[string]bool{}
	for _, v := range m {
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
