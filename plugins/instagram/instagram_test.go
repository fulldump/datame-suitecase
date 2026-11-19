package instagram_test

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
	"github.com/fulldump/datame-suitcase/plugins/instagram"
	"github.com/fulldump/datame-suitcase/plugintest"
)

// metaJSON marshals like Meta does: every byte of non-ASCII text escaped on
// its own, which reads back as mojibake («Ã©» for «é»).
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

func post(id string, day int, caption string) obj {
	return obj{"media": list{obj{"uri": "media/posts/202611/" + id + ".jpg", "creation_timestamp": ts(day, 10), "title": caption}},
		"title": caption, "creation_timestamp": ts(day, 10)}
}

func follower(name string, day int) obj {
	return obj{"title": "", "string_list_data": list{obj{"href": "https://www.instagram.com/" + name, "value": name, "timestamp": ts(day, 9)}}}
}

func message(sender string, day, hour int, content string, extra obj) obj {
	m := obj{"sender_name": sender, "timestamp_ms": ts(day, hour) * 1000, "content": content}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func thread(messages ...any) string {
	return metaJSON(obj{"participants": list{obj{"name": "Bea Ñúñez"}, obj{"name": "Ana Pérez"}},
		"messages": messages, "title": "Bea Ñúñez", "thread_path": "inbox/bea_123"})
}

const act = "your_instagram_activity/"

func export(posts list, followers list, messages string) map[string]string {
	return map[string]string{
		act + "content/posts_1.json": metaJSON(posts),
		act + "content/stories.json": metaJSON(obj{"ig_stories": list{obj{"uri": "media/stories/202611/222.mp4", "creation_timestamp": ts(3, 20), "title": ""}}}),
		act + "content/reels.json":   metaJSON(obj{"ig_reels_media": list{obj{"media": list{obj{"uri": "media/reels/202610/333.mp4", "creation_timestamp": ts(1, 0) - 3600, "title": "Reel de otoño"}}}}}),
		act + "comments/post_comments_1.json": metaJSON(list{obj{"string_map_data": obj{"Comment": obj{"value": "¡Qué bonito!"},
			"Media Owner": obj{"value": "bea"}, "Time": obj{"timestamp": ts(4, 12)}}}}),
		act + "likes/liked_posts.json":                                        metaJSON(obj{"likes_media_likes": list{obj{"title": "bea", "string_list_data": list{obj{"href": "https://www.instagram.com/p/abc/", "value": "❤", "timestamp": ts(5, 8)}}}}}),
		act + "saved/saved_posts.json":                                        metaJSON(obj{"saved_saved_media": list{obj{"title": "carlos", "string_map_data": obj{"Saved on": obj{"href": "https://www.instagram.com/p/def/", "timestamp": ts(6, 8)}}}}}),
		"connections/followers_and_following/followers_1.json":                metaJSON(followers),
		"connections/followers_and_following/following.json":                  metaJSON(obj{"relationships_following": list{obj{"title": "bea", "string_list_data": list{obj{"href": "https://www.instagram.com/bea", "timestamp": ts(2, 9)}}}}}),
		act + "messages/inbox/bea_123/message_1.json":                         messages,
		"personal_information/personal_information/personal_information.json": metaJSON(obj{"profile_user": list{obj{"string_map_data": obj{"Name": obj{"value": "Ana Pérez"}}}}}),
		"media/posts/202611/111.jpg":                                          "jpeg 111",
		"media/stories/202611/222.mp4":                                        "mp4 222",
		"media/reels/202610/333.mp4":                                          "mp4 333",
	}
}

func count(data []byte) int {
	var items []any
	if err := json.Unmarshal(data, &items); err != nil {
		panic(err)
	}
	return len(items)
}

func TestInstagramExportImport(t *testing.T) {
	drop := t.TempDir()
	photo := obj{"photos": list{obj{"uri": act + "messages/inbox/bea_123/photos/444.jpg", "creation_timestamp": ts(7, 11)}}}
	files := export(list{post("111", 2, "Café con Ana ☕")}, list{follower("bea", 1)},
		thread(message("Bea Ñúñez", 7, 10, "¿Vienes? 😀", obj{"reactions": list{obj{"reaction": "❤", "actor": "Ana Pérez"}}}),
			message("Ana Pérez", 7, 11, "", photo)))
	writeZip(t, filepath.Join(drop, "instagram-ana-2026-11-08-part1.zip"), files, 3*time.Hour)
	// The photo of the message came in the second ZIP of the same export.
	writeZip(t, filepath.Join(drop, "instagram-ana-2026-11-08-part2.zip"), map[string]string{
		act + "messages/inbox/bea_123/photos/444.jpg": "jpeg 444",
	}, 3*time.Hour-time.Minute)

	h := plugintest.New(t, instagram.Plugin{}, plugin.Config{"path": drop})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 10)

	posts := string(h.File("publicaciones/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(posts, "# Publicaciones: noviembre de 2026"))
	biff.AssertTrue(strings.Contains(posts, "Café con Ana ☕")) // the mojibake is repaired
	biff.AssertTrue(strings.Contains(posts, "![](../../media/2026/2026-11/111.jpg)"))
	biff.AssertTrue(strings.Contains(string(h.File("publicaciones/2026/2026-11.json")), "Café con Ana ☕"))
	biff.AssertEqual(string(h.File("media/2026/2026-11/111.jpg")), "jpeg 111")
	biff.AssertTrue(strings.Contains(string(h.File("historias/2026/2026-11.md")), "[vídeo: 222.mp4](../../media/2026/2026-11/222.mp4)"))
	biff.AssertTrue(strings.Contains(string(h.File("reels/2026/2026-10.md")), "Reel de otoño"))

	chat := string(h.File("mensajes/bea_123/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(chat, "# Bea Ñúñez: noviembre de 2026"))
	biff.AssertTrue(strings.Contains(chat, "**Bea Ñúñez** · 2026-11-07 10:00 UTC\n\n¿Vienes? 😀"))
	biff.AssertTrue(strings.Contains(chat, "_Reacciones: ❤ Ana Pérez_"))
	biff.AssertTrue(strings.Contains(chat, "![](../../../mensajes/bea_123/media/2026-11/444.jpg)"))
	biff.AssertEqual(string(h.File("mensajes/bea_123/media/2026-11/444.jpg")), "jpeg 444")
	biff.AssertTrue(h.Has("mensajes/bea_123/conversacion.json"))

	followers := string(h.File("seguidores.csv"))
	biff.AssertTrue(strings.HasPrefix(followers, "href,timestamp,value,title\n"))
	biff.AssertTrue(strings.Contains(followers, "https://www.instagram.com/bea,2026-11-01 09:00:00,bea,\n"))
	biff.AssertTrue(strings.Contains(string(h.File("comentarios.csv")), "¡Qué bonito!"))
	biff.AssertTrue(strings.Contains(string(h.File("me-gusta.csv")), "❤"))
	biff.AssertTrue(h.Has("guardados.json"))
	biff.AssertTrue(h.Has("seguidos.csv"))
	biff.AssertTrue(strings.Contains(string(h.File("otros/personal_information/personal_information/personal_information.json")), "Ana Pérez"))

	// The same again: nothing is written.
	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A newer export, unpacked: post 111 was deleted, there is a new post, a
	// new follower and a new message; the older ones stay.
	newer := export(list{post("112", 20, "Otra")}, list{follower("bea", 1), follower("carlos", 19)},
		thread(message("Bea Ñúñez", 7, 10, "¿Vienes? 😀", obj{"reactions": list{obj{"reaction": "❤", "actor": "Ana Pérez"}}}),
			message("Ana Pérez", 7, 11, "", photo),
			message("Bea Ñúñez", 20, 9, "Nuevo", nil)))
	delete(newer, "media/posts/202611/111.jpg")
	newer["media/posts/202611/112.jpg"] = "jpeg 112"
	dir := filepath.Join(drop, "instagram-ana-2026-11-21")
	for name, content := range newer {
		biff.AssertNil(os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
		biff.AssertNil(os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 3)
	biff.AssertEqual(count(h.File("publicaciones/2026/2026-11.json")), 2)
	biff.AssertEqual(count(h.File("seguidores.json")), 2)
	biff.AssertEqual(count(h.File("mensajes/bea_123/2026/2026-11.json")), 3)
	// The photo of the message is still linked, though this export lacks it.
	biff.AssertTrue(strings.Contains(string(h.File("mensajes/bea_123/2026/2026-11.md")), "media/2026-11/444.jpg)"))
	stories, _ := h.NAS.File("principal", "/Archivo/historias/2026/2026-11.json")
	biff.AssertEqual(stories.Versions, 1)
}

func TestInstagramRejectsHTML(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "instagram-html.zip")
	writeZip(t, file, map[string]string{"start_here.html": "<html></html>", "your_instagram_activity/content/posts_1.html": "<html></html>"}, 0)
	h := plugintest.New(t, instagram.Plugin{}, plugin.Config{"path": file})
	err := h.Validate()
	biff.AssertNotNil(err)
	biff.AssertTrue(strings.Contains(err.Error(), "JSON"))
}
