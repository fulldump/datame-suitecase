package takeout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

// Google Photos in Takeout: one folder per album and one per year («Photos
// from 2023», «Fotos de 2023») with every photo, and next to each photo a
// JSON with its date, place and description. The same photo comes once per
// album it is in, and again in its year.
//
// In the NAS each photo is stored once, by when it was taken:
// Fotos/<AAAA>/<AAAA-MM>/<name>, with the JSON beside it and that date as its
// modification time. Albums are lists (album.md and album.json) that link to
// those files, in Fotos/Albumes/<album>/.

var yearFolder = regexp.MustCompile(`(?i)^(photos from|fotos de|fotos del) (\d{4})$`)

// editedSuffixes mark the edited copy of a photo, which shares the JSON of
// the original: «IMG_1-edited.jpg» uses «IMG_1.jpg.json».
var editedSuffixes = []string{"-edited", "-editado", "-editada", "-bearbeitet", "-modifié", "-modificato"}

var counter = regexp.MustCompile(`^(.*)\((\d+)\)(\.[^.()]*)?$`)

func isAlbumMetadata(base string) bool {
	lower := strings.ToLower(base)
	return lower == "metadata.json" || lower == "metadatos.json"
}

// findSidecar finds the JSON of a photo among the files of its folder.
// Takeout names it «<photo>.json», or «<photo>.supplemental-metadata.json» in
// newer exports, but cuts long names (to about 46 characters before
// «.json»), and for the second photo with a name it writes «IMG(1).jpg» and
// «IMG.jpg(1).json», with the number at the end.
func findSidecar(files map[string]*entry, base string) *entry {
	type try struct{ name, n string }
	tries := []try{{base, ""}}
	if m := counter.FindStringSubmatch(base); m != nil {
		tries = append(tries, try{m[1] + m[3], m[2]})
	}
	for _, t := range tries {
		names := []string{t.name}
		ext := path.Ext(t.name)
		stem := strings.TrimSuffix(t.name, ext)
		for _, suffix := range editedSuffixes {
			if strings.HasSuffix(strings.ToLower(stem), suffix) {
				names = append(names, stem[:len(stem)-len(suffix)]+ext)
			}
		}
		number := ""
		if t.n != "" {
			number = "(" + t.n + ")"
		}
		for _, name := range names {
			full := name + ".supplemental-metadata"
			// From the whole name down to the name without extension, or to
			// 40 bytes for long names, which Google cuts to 46 characters
			// even inside the name: the longest that exists is the one
			// Google wrote.
			shortest := min(len(strings.TrimSuffix(name, path.Ext(name))), 40)
			for l := len(full); l >= shortest && l > 0; l-- {
				if e := files[full[:l]+number+".json"]; e != nil {
					return e
				}
			}
		}
	}
	return nil
}

type sidecar struct {
	Title          string `json:"title"`
	Description    string `json:"description"`
	PhotoTakenTime struct {
		Timestamp string `json:"timestamp"`
	} `json:"photoTakenTime"`
	CreationTime struct {
		Timestamp string `json:"timestamp"`
	} `json:"creationTime"`
}

func (s sidecar) taken() time.Time {
	for _, ts := range []string{s.PhotoTakenTime.Timestamp, s.CreationTime.Timestamp} {
		if seconds, err := strconv.ParseInt(ts, 10, 64); err == nil && seconds > 0 {
			return time.Unix(seconds, 0).UTC()
		}
	}
	return time.Time{}
}

// photoPath is where a photo goes, relative to Fotos/.
func photoPath(taken time.Time, year, base string) string {
	name := cleanName(base)
	switch {
	case !taken.IsZero():
		return fmt.Sprintf("%04d/%s/%s", taken.Year(), taken.Format("2006-01"), name)
	case year != "":
		return year + "/sin-fecha/" + name
	}
	return "sin-fecha/" + name
}

// withHash renames a photo that has the name of another one.
func withHash(p, hash string) string {
	ext := path.Ext(p)
	return strings.TrimSuffix(p, ext) + "-" + hash[:8] + ext
}

func isVideo(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".mov", ".m4v", ".3gp", ".avi", ".mkv", ".webm", ".mpg", ".mpeg", ".mts", ".m2ts", ".wmv":
		return true
	}
	return false
}

func (x *importer) photo(ctx context.Context, e *entry, rel string) error {
	dir, base := path.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	top, _ := e.top()
	if dir == "" || strings.Contains(dir, "/") {
		// Files of the product itself (print orders, shared album comments).
		_, err := x.raw(ctx, e, "Fotos/otros/"+cleanPath(rel), "")
		return err
	}
	if strings.EqualFold(path.Ext(base), ".json") {
		// Decided at the end: the JSON of a photo, an album's, or a lonely one.
		x.jsons[e.name] = e
		return nil
	}
	year := ""
	if m := yearFolder.FindStringSubmatch(dir); m != nil {
		year = m[2]
	}
	var meta sidecar
	var metaData []byte
	sc := findSidecar(x.idx.dirs[top+"/"+dir], base)
	if sc != nil {
		data, err := sc.readSmall(16 << 20)
		if err != nil {
			return err
		}
		if json.Unmarshal(data, &meta) == nil {
			x.sidecars[sc.name] = true
			metaData = data
		}
	}
	taken := meta.taken()

	sum, err := contentHash(e)
	if err != nil {
		return fmt.Errorf("%s: %w", e.name, err)
	}
	hash := sum[:16]
	stored, known := x.c.Photos[hash]
	if !known {
		stored = photoPath(taken, year, base)
		if other, ok := x.taken[stored]; ok && other != hash {
			stored = withHash(stored, hash)
		}
		modTime := taken
		if modTime.IsZero() {
			modTime = e.modTime
		}
		file := plugin.File{Path: "Fotos/" + stored, ModTime: modTime, Size: e.size, Open: e.open}
		result, err := x.run.Out.Write(ctx, file)
		if err == nil && result == plugin.Kept {
			// Another photo with this name was there before (the list of
			// photos was lost): this one gets a name of its own.
			stored = withHash(stored, hash)
			file.Path = "Fotos/" + stored
			result, err = x.run.Out.Write(ctx, file)
		}
		if err != nil {
			return err
		}
		x.created("fotos y vídeos", result)
		x.c.Photos[hash] = stored
		x.taken[stored] = hash
	}
	if metaData != nil && !x.written[stored] {
		x.written[stored] = true
		modTime := taken
		if modTime.IsZero() {
			modTime = e.modTime
		}
		// Mutable: a description or a person tagged later changes it.
		if _, err := x.run.Out.Write(ctx, plugin.File{Path: "Fotos/" + stored + ".json", ModTime: modTime, Data: metaData, Mutable: true}); err != nil {
			return err
		}
	}
	if year == "" {
		key := top + "/" + dir
		if x.albums[key] == nil {
			x.albums[key] = map[string]bool{}
		}
		x.albums[key][stored] = true
	}
	return nil
}

type albumFile struct {
	Title       string   `json:"titulo"`
	Description string   `json:"descripcion,omitempty"`
	Photos      []string `json:"fotos"` // relative to Fotos/
}

func (x *importer) finishPhotos(ctx context.Context) error {
	// JSON files that are not of a photo nor of an album are kept as they are.
	names := make([]string, 0, len(x.jsons))
	for name := range x.jsons {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := x.jsons[name]
		_, rest := e.top()
		if x.sidecars[name] || isAlbumMetadata(path.Base(rest)) {
			continue
		}
		if _, err := x.raw(ctx, e, "Fotos/otros/"+cleanPath(rest), ""); err != nil {
			return err
		}
	}

	keys := make([]string, 0, len(x.albums))
	for key := range x.albums {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := x.writeAlbum(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (x *importer) writeAlbum(ctx context.Context, key string) error {
	folderName := path.Base(key)
	folder := "Fotos/Albumes/" + cleanName(folderName)
	album := albumFile{Title: folderName}
	for base, e := range x.idx.dirs[key] {
		if !isAlbumMetadata(base) {
			continue
		}
		data, err := e.readSmall(16 << 20)
		if err != nil {
			return err
		}
		var meta struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		if json.Unmarshal(data, &meta) == nil {
			if meta.Title != "" {
				album.Title = meta.Title
			}
			album.Description = meta.Description
		}
		if _, err := x.run.Out.Write(ctx, plugin.File{Path: folder + "/metadata.json", ModTime: e.modTime, Data: data, Mutable: true}); err != nil {
			return err
		}
		break
	}

	// An album only grows: photos taken out of it in Google stay in the list.
	photos := x.albums[key]
	existing, err := x.run.Out.Read(ctx, folder+"/album.json")
	if err != nil && !errors.Is(err, plugin.ErrNotExist) {
		return err
	}
	if err == nil {
		var old albumFile
		if json.Unmarshal(existing, &old) == nil {
			for _, p := range old.Photos {
				photos[p] = true
			}
		}
	}
	album.Photos = make([]string, 0, len(photos))
	for p := range photos {
		album.Photos = append(album.Photos, p)
	}
	sort.Strings(album.Photos)

	var md bytes.Buffer
	fmt.Fprintf(&md, "# Álbum: %s\n\n", album.Title)
	if album.Description != "" {
		fmt.Fprintf(&md, "%s\n\n", album.Description)
	}
	fmt.Fprintf(&md, "%d fotos y vídeos. Cada uno está guardado una sola vez, en la carpeta de su año.\n\n", len(album.Photos))
	for _, p := range album.Photos {
		// From Fotos/Albumes/<album>/ up to Fotos/.
		link := "../../" + markdownPath(p)
		if isVideo(p) {
			fmt.Fprintf(&md, "- [vídeo: %s](%s)\n", path.Base(p), link)
		} else {
			fmt.Fprintf(&md, "- ![%s](%s)\n", path.Base(p), link)
		}
	}
	modTime := x.run.Now()
	if _, err := x.run.Out.Write(ctx, plugin.File{Path: folder + "/album.json", ModTime: modTime, Data: plugin.JSON(album), Mutable: true}); err != nil {
		return err
	}
	_, err = x.run.Out.Write(ctx, plugin.File{Path: folder + "/album.md", ModTime: modTime, Data: md.Bytes(), Mutable: true})
	return err
}

// markdownPath escapes what would break a Markdown link: spaces and brackets.
func markdownPath(p string) string {
	return strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E").Replace(p)
}
