package twitter_test

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/twitter"
	"github.com/fulldump/datame-suitcase/plugintest"
)

type fakeTweet struct {
	id, created, text string
}

// writeArchive makes a small synthetic archive with the layout X uses.
func writeArchive(t *testing.T, file string, tweets []fakeTweet, media map[string]string) {
	t.Helper()
	f, err := os.Create(file)
	biff.AssertNil(err)
	z := zip.NewWriter(f)
	add := func(name, content string) {
		w, err := z.Create(name)
		biff.AssertNil(err)
		w.Write([]byte(content))
	}
	add("data/account.js", `window.YTD.account.part0 = [{"account":{"username":"ana_prueba","accountId":"1"}}]`)
	items := []map[string]any{}
	for _, tw := range tweets {
		items = append(items, map[string]any{"tweet": map[string]any{
			"id_str": tw.id, "id": tw.id, "created_at": tw.created, "full_text": tw.text + " https://t.co/abc",
			"favorite_count": "3", "retweet_count": "0",
			"entities": map[string]any{"urls": []any{map[string]any{"url": "https://t.co/abc", "expanded_url": "https://ejemplo.org/largo"}}},
		}})
	}
	data, _ := json.Marshal(items)
	add("data/tweets.js", "window.YTD.tweets.part0 = "+string(data))
	add("data/like.js", `window.YTD.like.part0 = [{"like":{"tweetId":"999","fullText":"Algo que me gustó","expandedUrl":"https://x.com/i/web/status/999"}}]`)
	add("data/README.txt", "synthetic")
	for name, content := range media {
		add("data/tweets_media/"+name, content)
	}
	biff.AssertNil(z.Close())
	biff.AssertNil(f.Close())
}

func monthIDs(t *testing.T, data []byte) []string {
	var list []struct {
		ID string `json:"id_str"`
	}
	biff.AssertNil(json.Unmarshal(data, &list))
	ids := []string{}
	for _, item := range list {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestTwitterArchiveImport(t *testing.T) {
	drop := t.TempDir()
	writeArchive(t, filepath.Join(drop, "twitter-2025-01-10-aaa.zip"), []fakeTweet{
		{"100", "Fri Dec 20 10:00:00 +0000 2024", "Hola\nmundo"},
		{"101", "Sat Jan 04 09:30:00 +0000 2025", "Primer tuit del año"},
		{"102", "Sun Jan 05 18:00:00 +0000 2025", "Una foto"},
	}, map[string]string{"102-Fotito.jpg": "jpeg bytes"})

	h := plugintest.New(t, twitter.Plugin{}, plugin.Config{"path": drop, "keep_zip": "true"})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 3)

	biff.AssertEqual(monthIDs(t, h.File("tweets/2025/2025-01.json")), []string{"101", "102"})
	md := string(h.File("tweets/2025/2025-01.md"))
	biff.AssertTrue(strings.Contains(md, "# Tuits de @ana_prueba: enero de 2025"))
	biff.AssertTrue(strings.Contains(md, "https://ejemplo.org/largo"))
	biff.AssertTrue(strings.Contains(md, "![](../../media/2025/2025-01/102-Fotito.jpg)"))
	biff.AssertTrue(strings.Contains(string(h.File("tweets/2024/2024-12.md")), "Hola  \nmundo"))
	biff.AssertEqual(string(h.File("media/2025/2025-01/102-Fotito.jpg")), "jpeg bytes")
	biff.AssertTrue(h.Has("originales/twitter-2025-01-10-aaa.zip"))
	biff.AssertTrue(strings.Contains(string(h.File("me-gusta.md")), "Algo que me gustó"))

	// Same folder, nothing new: nothing is read or written again.
	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A newer archive, where tweet 101 was deleted in X and 103 is new: the copy
	// keeps 101.
	writeArchive(t, filepath.Join(drop, "twitter-2025-02-01-bbb.zip"), []fakeTweet{
		{"100", "Fri Dec 20 10:00:00 +0000 2024", "Hola\nmundo"},
		{"102", "Sun Jan 05 18:00:00 +0000 2025", "Una foto"},
		{"103", "Tue Jan 28 07:00:00 +0000 2025", "Nuevo"},
	}, map[string]string{"102-Fotito.jpg": "jpeg bytes"})
	created := h.NAS.Created
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertEqual(monthIDs(t, h.File("tweets/2025/2025-01.json")), []string{"101", "102", "103"})
	biff.AssertEqual(h.NAS.Created-created, 1) // only the new ZIP; the media was there
	file, _ := h.NAS.File("principal", "/Archivo/tweets/2024/2024-12.json")
	biff.AssertEqual(file.Versions, 1) // a month that did not change is not touched

	// A whole new install over the same folder writes nothing new.
	h.Cursor, h.Manifest = nil, map[string]string{}
	created, replaced := h.NAS.Created, h.NAS.Replace
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Created, created)
	biff.AssertEqual(h.NAS.Replace, replaced)
}

func TestTwitterRejectsOtherZips(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "otra-cosa.zip")
	f, _ := os.Create(file)
	z := zip.NewWriter(f)
	w, _ := z.Create("hola.txt")
	fmt.Fprint(w, "hola")
	z.Close()
	f.Close()
	h := plugintest.New(t, twitter.Plugin{}, plugin.Config{"path": file})
	biff.AssertNotNil(h.Validate())
	h.Config["path"] = filepath.Join(dir, "no-existe")
	biff.AssertNotNil(h.Validate())
}
