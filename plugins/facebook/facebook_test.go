package facebook_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/facebook"
	"github.com/fulldump/datame-suitcase/plugintest"
)

// metaJSON marshals like Meta does: every byte of non-ASCII text escaped on
// its own, which reads back as mojibake.
func metaJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var b bytes.Buffer
	for _, c := range data {
		if c >= 0x80 {
			fmt.Fprintf(&b, "\\u%04x", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func writeZip(t *testing.T, file string, files map[string]string, age time.Duration) {
	t.Helper()
	f, err := os.Create(file)
	biff.AssertNil(err)
	z := zip.NewWriter(f)
	for name, content := range files {
		w, err := z.Create(name)
		biff.AssertNil(err)
		w.Write([]byte(content))
	}
	biff.AssertNil(z.Close())
	biff.AssertNil(f.Close())
	when := time.Now().Add(-age)
	biff.AssertNil(os.Chtimes(file, when, when))
}

func ts(day, hour int) int64 { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC).Unix() }

type obj = map[string]any
type list = []any

const act = "your_facebook_activity/"
const media = act + "posts/media/Cadiz_abc/"

func photo(id string, day int, title string) obj {
	return obj{"uri": media + id + ".jpg", "creation_timestamp": ts(day, 12), "title": title}
}

func export(photos list, friends list) map[string]string {
	files := map[string]string{
		act + "posts/your_posts__check_ins__photos_and_videos_1.json": metaJSON(list{obj{
			"timestamp": ts(2, 18),
			"data":      list{obj{"post": "Hoy he ido a Cádiz"}, obj{"update_timestamp": ts(2, 18)}},
			"attachments": list{obj{"data": list{
				obj{"media": obj{"uri": media + "555.jpg", "creation_timestamp": ts(2, 12), "title": ""}},
				obj{"external_context": obj{"url": "https://ejemplo.org/cadiz"}},
			}}},
			"title": "Ana Pérez ha actualizado su estado.",
		}}),
		act + "posts/album/0.json":     metaJSON(obj{"name": "Cádiz 2026", "photos": photos, "description": "Vacaciones en el sur", "last_modified_timestamp": ts(3, 0)}),
		act + "posts/your_videos.json": metaJSON(obj{"videos_v2": list{obj{"uri": act + "posts/media/videos/777.mp4", "creation_timestamp": ts(4, 9), "description": "Olas"}}}),
		act + "comments_and_reactions/comments.json": metaJSON(obj{"comments_v2": list{obj{"timestamp": ts(5, 10),
			"data": list{obj{"comment": obj{"timestamp": ts(5, 10), "comment": "¡Enhorabuena!", "author": "Ana Pérez"}}}, "title": "Ana Pérez ha comentado la publicación de Bea."}}}),
		act + "comments_and_reactions/likes_and_reactions_1.json": metaJSON(list{obj{"timestamp": ts(6, 10),
			"data": list{obj{"reaction": obj{"reaction": "LIKE", "actor": "Ana Pérez"}}}, "title": "A Ana Pérez le gusta la publicación de Bea."}}),
		"connections/friends/your_friends.json": metaJSON(obj{"friends_v2": friends}),
		act + "messages/inbox/bea_456/message_1.json": metaJSON(obj{"participants": list{obj{"name": "Bea Ñúñez"}, obj{"name": "Ana Pérez"}},
			"messages": list{obj{"sender_name": "Bea Ñúñez", "timestamp_ms": ts(7, 10) * 1000, "content": "¿Qué tal el viaje?", "share": obj{"link": "https://ejemplo.org/x"}}},
			"title":    "Bea Ñúñez", "thread_path": "inbox/bea_456"}),
		act + "posts/media/videos/777.mp4": "mp4 777",
	}
	for _, p := range photos {
		uri := p.(obj)["uri"].(string)
		files[uri] = "jpeg " + filepath.Base(uri)
	}
	return files
}

func count(data []byte) int {
	var items []any
	if err := json.Unmarshal(data, &items); err != nil {
		panic(err)
	}
	return len(items)
}

func TestFacebookExportImport(t *testing.T) {
	drop := t.TempDir()
	writeZip(t, filepath.Join(drop, "facebook-ana-2026-11-08.zip"),
		export(list{photo("555", 2, "Playa"), photo("666", 2, "Atardecer")}, list{obj{"name": "Bea Ñúñez", "timestamp": ts(1, 8)}}), 2*time.Hour)

	h := plugintest.New(t, facebook.Plugin{}, plugin.Config{"path": drop})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	// 1 post, 2 photos, 1 video, 1 comment, 1 reaction, 1 friend, 1 message.
	biff.AssertEqual(r.Items, 8)

	posts := string(h.File("publicaciones/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(posts, "_Ana Pérez ha actualizado su estado._"))
	biff.AssertTrue(strings.Contains(posts, "Hoy he ido a Cádiz"))
	biff.AssertTrue(strings.Contains(posts, "![](../../media/2026/2026-11/555.jpg)"))
	biff.AssertTrue(strings.Contains(posts, "<https://ejemplo.org/cadiz>"))

	album := string(h.File("albumes/cadiz-2026/album.md"))
	biff.AssertTrue(strings.Contains(album, "# Cádiz 2026\n\nVacaciones en el sur"))
	biff.AssertTrue(strings.Contains(album, "Playa\n\n![](../../albumes/cadiz-2026/555.jpg)"))
	biff.AssertEqual(string(h.File("albumes/cadiz-2026/666.jpg")), "jpeg 666.jpg")
	biff.AssertTrue(strings.Contains(string(h.File("albumes/cadiz-2026/album.json")), "Vacaciones en el sur"))
	biff.AssertTrue(strings.Contains(string(h.File("albumes/videos/album.md")), "[vídeo: 777.mp4](../../albumes/videos/777.mp4)"))

	biff.AssertTrue(strings.Contains(string(h.File("comentarios.csv")), "¡Enhorabuena!"))
	biff.AssertTrue(strings.Contains(string(h.File("reacciones.csv")), "LIKE"))
	biff.AssertEqual(string(h.File("amigos.csv")), "name,timestamp\nBea Ñúñez,2026-11-01 08:00:00\n")
	chat := string(h.File("mensajes/bea_456/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(chat, "**Bea Ñúñez** · 2026-11-07 10:00 UTC\n\n¿Qué tal el viaje?"))
	biff.AssertTrue(strings.Contains(chat, "<https://ejemplo.org/x>"))

	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A newer export: photo 666 was deleted from the album and 888 added; one
	// friend more. Nothing that was there goes away.
	writeZip(t, filepath.Join(drop, "facebook-ana-2026-11-20.zip"),
		export(list{photo("555", 2, "Playa"), photo("888", 15, "Nuevo")},
			list{obj{"name": "Carlos", "timestamp": ts(14, 8)}, obj{"name": "Bea Ñúñez", "timestamp": ts(1, 8)}}), time.Hour)
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2)
	biff.AssertEqual(count(h.File("albumes/cadiz-2026/fotos.json")), 3)
	biff.AssertEqual(count(h.File("amigos.json")), 2)
	biff.AssertEqual(count(h.File("publicaciones/2026/2026-11.json")), 1)
	biff.AssertTrue(strings.Contains(string(h.File("albumes/cadiz-2026/album.md")), "666.jpg"))
}
