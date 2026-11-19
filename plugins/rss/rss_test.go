package rss_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/rss"
	"github.com/fulldump/datame-suitcase/plugintest"
)

// blog is a fake blog: a feed that can change, and one image.
type blog struct {
	mutex   sync.Mutex
	feed    string
	version int
	hits    map[string]int
}

func (b *blog) set(feed string) {
	b.mutex.Lock()
	b.feed, b.version = feed, b.version+1
	b.mutex.Unlock()
}

func (b *blog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if b.hits == nil {
		b.hits = map[string]int{}
	}
	b.hits[r.URL.Path]++
	switch r.URL.Path {
	case "/feed":
		etag := fmt.Sprintf(`"v%d"`, b.version)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, b.feed)
	case "/img/gato.png":
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG fake image"))
	default:
		http.NotFound(w, r)
	}
}

const rssFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel><title>Mi blog</title>
<item>
  <title>¡Hola, Año Nuevo!</title>
  <link>%[1]s/2025/12/31/hola-ano-nuevo/</link>
  <guid>post-1</guid>
  <pubDate>Wed, 31 Dec 2025 10:00:00 +0000</pubDate>
  <dc:creator>Ana</dc:creator>
  <category>vida</category>
  <content:encoded><![CDATA[<p>Primer párrafo%[2]s.</p><p><img src="/img/gato.png" alt="gato"></p>]]></content:encoded>
</item>
<item>
  <title>Sin fecha</title>
  <link>%[1]s/sin-fecha</link>
  <description>&lt;p&gt;Texto &amp;amp; más&lt;/p&gt;</description>
</item>
%[3]s
</channel></rss>`

func TestRSSKeepsEachPostOnce(t *testing.T) {
	b := &blog{}
	server := httptest.NewServer(b)
	defer server.Close()
	b.set(fmt.Sprintf(rssFeed, server.URL, "", ""))

	h := plugintest.New(t, rss.Plugin{}, plugin.Config{"url": server.URL + "/feed", "images": "true"})
	biff.AssertNil(h.Validate())

	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2)
	md := string(h.File("2025/2025-12-31-hola-ano-nuevo/index.md"))
	biff.AssertTrue(strings.Contains(md, "# ¡Hola, Año Nuevo!"))
	biff.AssertTrue(strings.Contains(md, "Primer párrafo."))
	biff.AssertTrue(strings.Contains(md, "- Autor: Ana"))
	page := string(h.File("2025/2025-12-31-hola-ano-nuevo/index.html"))
	biff.AssertTrue(strings.Contains(page, `src="media/`))
	biff.AssertTrue(strings.Contains(string(h.File("2025/2025-12-31-hola-ano-nuevo/index.json")), `"title": "¡Hola, Año Nuevo!"`))
	var image string
	for _, p := range h.NAS.Paths("principal") {
		if strings.Contains(p, "/media/") {
			image = p
		}
	}
	biff.AssertTrue(strings.HasSuffix(image, "-gato.png"))
	// The undated post takes the day it was first seen.
	biff.AssertTrue(strings.Contains(string(h.File("2026/2026-01-02-sin-fecha/index.md")), "Texto & más"))

	// Nothing new: the server answers 304 and nothing is written.
	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A post is edited and another appears. The edited one is replaced (the NAS
	// keeps the old version), its image is not downloaded again, and the undated
	// post keeps its folder although days have passed.
	h.Now = h.Now.AddDate(0, 0, 5)
	b.set(fmt.Sprintf(rssFeed, server.URL, " (editado)", `<item><title>Otra</title><link>`+server.URL+`/otra</link><guid>post-3</guid><pubDate>2026-01-05T08:00:00Z</pubDate></item>`))
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertTrue(strings.Contains(string(h.File("2025/2025-12-31-hola-ano-nuevo/index.md")), "(editado)"))
	biff.AssertTrue(h.Has("2026/2026-01-05-otra/index.md"))
	biff.AssertFalse(h.Has("2026/2026-01-07-sin-fecha/index.md"))
	b.mutex.Lock()
	biff.AssertEqual(b.hits["/img/gato.png"], 1)
	b.mutex.Unlock()

	// Lost state (a new install pointing at the same folder): nothing is
	// duplicated, except the undated post, which can only be dated again by the
	// day it is seen and so lands in a new folder (its three files).
	h.Cursor, h.Manifest = nil, map[string]string{}
	created := h.NAS.Created
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(h.NAS.Created-created, 3)
	biff.AssertTrue(h.Has("2026/2026-01-07-sin-fecha/index.md"))
}

func TestAtomFeed(t *testing.T) {
	b := &blog{}
	server := httptest.NewServer(b)
	defer server.Close()
	b.set(`<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom</title>
<entry><title type="html">Uno &amp;lt;b&amp;gt;</title><id>tag:x,2024:1</id>
<link rel="alternate" href="https://ejemplo.org/uno.html"/>
<published>2024-03-01T10:00:00Z</published><updated>2024-03-02T10:00:00Z</updated>
<content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Hola <b>mundo</b></p></div></content>
</entry></feed>`)

	h := plugintest.New(t, rss.Plugin{}, plugin.Config{"url": server.URL + "/feed", "images": "false"})
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	md := string(h.File("2024/2024-03-01-uno/index.md"))
	biff.AssertTrue(strings.Contains(md, "Hola mundo"))
}

func TestNotAFeed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>hola</body></html>")
	}))
	defer server.Close()
	h := plugintest.New(t, rss.Plugin{}, plugin.Config{"url": server.URL})
	biff.AssertNotNil(h.Validate())
	h.Config["url"] = "ftp://x"
	biff.AssertNotNil(h.Validate())
}
