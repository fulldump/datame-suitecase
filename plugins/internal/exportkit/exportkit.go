// Package exportkit holds what the importers of downloaded exports share: how
// to find them (a ZIP, an unpacked folder, or a drop folder where new ones
// keep arriving), how to remember which ones were imported, and how to merge a
// month of items with what is already on the NAS so that a newer export adds
// and never removes.
//
// The X and Telegram importers predate it and keep their own copies of these
// ideas; the output of this package follows the same conventions (monthly
// JSON + Markdown, mutable months, media written once under a stable name).
package exportkit

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

// Source is one export: a file (usually a ZIP) or an unpacked folder.
type Source struct {
	Path string
	Key  string // changes when the export changes, so a re-downloaded one is imported again
	Dir  bool
	mod  time.Time
}

// Name is the base name of the export, for logs and for naming originals.
func (s Source) Name() string { return filepath.Base(s.Path) }

// Finder tells exports apart from anything else in a folder.
type Finder struct {
	// IsExport says whether an unpacked folder is an export.
	IsExport func(fsys fs.FS) bool
	// Accept says whether a loose file is an export. Nil means «a .zip».
	Accept func(name string) bool
}

func (f Finder) accept(name string) bool {
	if f.Accept != nil {
		return f.Accept(name)
	}
	return strings.EqualFold(filepath.Ext(name), ".zip")
}

func fileKey(name string, info os.FileInfo) string {
	return fmt.Sprintf("%s|%d|%d", name, info.Size(), info.ModTime().Unix())
}

// dirKey sums every file of a folder: an unpacked export has no single file
// that changes when the export is replaced by a newer one.
func dirKey(dir string) (string, time.Time) {
	var count, size int64
	var newest time.Time
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			count++
			size += info.Size()
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		return nil
	})
	return fmt.Sprintf("%s/|%d|%d|%d", filepath.Base(dir), count, size, newest.Unix()), newest
}

// Find lists the exports in root: root itself when it is one (a file or an
// unpacked folder), or else the exports directly inside it, oldest first so
// that a newer export merges over an older one.
func (f Finder) Find(root string) ([]Source, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("no encuentro %s", root)
	}
	if !info.IsDir() {
		return []Source{{Path: root, Key: fileKey(filepath.Base(root), info), mod: info.ModTime()}}, nil
	}
	if f.IsExport != nil && f.IsExport(os.DirFS(root)) {
		key, mod := dirKey(root)
		return []Source{{Path: root, Key: key, Dir: true, mod: mod}}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := []Source{}
	for _, entry := range entries {
		full := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			if f.IsExport != nil && f.IsExport(os.DirFS(full)) {
				key, mod := dirKey(full)
				out = append(out, Source{Path: full, Key: key, Dir: true, mod: mod})
			}
			continue
		}
		if !f.accept(entry.Name()) {
			continue
		}
		if i, err := entry.Info(); err == nil {
			out = append(out, Source{Path: full, Key: fileKey(entry.Name(), i), mod: i.ModTime()})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].mod.Equal(out[j].mod) {
			return out[i].mod.Before(out[j].mod)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// IsZip tells a ZIP by its first bytes, not by its name: browsers and phones
// rename downloads.
func IsZip(file string) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	return n == 4 && bytes.Equal(head, []byte("PK\x03\x04"))
}

// Open returns the export as a file system: the ZIP, the folder, or for a
// loose file the folder that holds it (a WhatsApp .txt has its media beside
// it).
func (s Source) Open() (fs.FS, func() error, error) {
	if s.Dir {
		return os.DirFS(s.Path), func() error { return nil }, nil
	}
	if IsZip(s.Path) {
		r, err := zip.OpenReader(s.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("%s no es un ZIP válido", s.Name())
		}
		return r, r.Close, nil
	}
	return os.DirFS(filepath.Dir(s.Path)), func() error { return nil }, nil
}

type cursor struct {
	Imported map[string]int64 `json:"imported"` // source key → when
}

// Sync imports every export of root not imported yet, saving the cursor after
// each one so that a run cut half way does not start again.
func Sync(ctx context.Context, run *plugin.Run, finder Finder, importOne func(context.Context, *plugin.Run, Source, []Source) error) error {
	c := cursor{Imported: map[string]int64{}}
	if err := run.Load(&c); err != nil {
		return err
	}
	if c.Imported == nil {
		c.Imported = map[string]int64{}
	}
	list, err := finder.Find(run.Config.Get("path"))
	if err != nil {
		return err
	}
	pending := 0
	for _, s := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.Imported[s.Key] != 0 {
			continue
		}
		pending++
		run.Logf("Importando %s", s.Name())
		if err := importOne(ctx, run, s, list); err != nil {
			return fmt.Errorf("%s: %w", s.Name(), err)
		}
		c.Imported[s.Key] = run.Now().Unix()
		if err := run.Save(c); err != nil {
			return err
		}
	}
	if pending == 0 {
		run.Logf("No hay exportaciones nuevas que importar")
	}
	return nil
}

// ---- Merging ----

// Record is one item of a month (a post, a message), as it will be written in
// the JSON, with the key that tells whether two exports have the same item.
type Record struct {
	Key  string
	Time time.Time
	Raw  json.RawMessage
}

// Merge reads the JSON list at jsonPath on the NAS, adds the fresh records by
// key and returns the whole list in time order, with how many were new.
// parse turns a stored item back into a Record. combine picks what to keep
// when an item is in both; nil means the fresh one, which is newer.
func Merge(ctx context.Context, out plugin.Output, jsonPath string, fresh []Record,
	parse func(json.RawMessage) (Record, bool), combine func(old, fresh Record) Record) ([]Record, int, error) {
	merged := map[string]Record{}
	existing, err := out.Read(ctx, jsonPath)
	if err != nil && !errors.Is(err, plugin.ErrNotExist) {
		return nil, 0, err
	}
	if err == nil {
		var old []json.RawMessage
		if err := json.Unmarshal(existing, &old); err != nil {
			return nil, 0, fmt.Errorf("%s en el NAS no se puede leer: %w", jsonPath, err)
		}
		for _, raw := range old {
			if r, ok := parse(raw); ok {
				merged[r.Key] = r
			}
		}
	}
	added := 0
	for _, r := range fresh {
		old, ok := merged[r.Key]
		switch {
		case !ok:
			added++
		case combine != nil:
			r = combine(old, r)
		}
		merged[r.Key] = r
	}
	list := make([]Record, 0, len(merged))
	for _, r := range merged {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Time.Equal(list[j].Time) {
			return list[i].Time.Before(list[j].Time)
		}
		return list[i].Key < list[j].Key
	})
	return list, added, nil
}

// Raws returns the items of a merged list, to write as JSON.
func Raws(list []Record) []json.RawMessage {
	out := make([]json.RawMessage, len(list))
	for i, r := range list {
		out[i] = r.Raw
	}
	return out
}

// WriteMonth writes a month as JSON and Markdown, both mutable: a month grows
// with each export and the NAS keeps the version before.
func WriteMonth(ctx context.Context, run *plugin.Run, jsonPath string, list []Record, md []byte) error {
	if len(list) == 0 {
		return nil
	}
	modTime := list[len(list)-1].Time
	if _, err := run.Out.Write(ctx, plugin.File{Path: jsonPath, ModTime: modTime, Data: plugin.JSON(Raws(list)), Mutable: true}); err != nil {
		return err
	}
	_, err := run.Out.Write(ctx, plugin.File{Path: strings.TrimSuffix(jsonPath, ".json") + ".md", ModTime: modTime, Data: md, Mutable: true})
	return err
}

// GroupByMonth splits records by «AAAA-MM», months in order.
func GroupByMonth(records []Record) ([]string, map[string][]Record) {
	byMonth := map[string][]Record{}
	for _, r := range records {
		m := r.Time.Format("2006-01")
		byMonth[m] = append(byMonth[m], r)
	}
	months := make([]string, 0, len(byMonth))
	for m := range byMonth {
		months = append(months, m)
	}
	sort.Strings(months)
	return months, byMonth
}

// MonthPath is «<folder>/AAAA/AAAA-MM.json».
func MonthPath(folder, month string) string {
	return path.Join(folder, month[:4], month+".json")
}

var monthNames = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

// MonthTitle turns «2026-11» into «noviembre de 2026».
func MonthTitle(month string) string {
	var n int
	fmt.Sscanf(month[5:], "%d", &n)
	if n < 1 || n > 12 {
		return month
	}
	return monthNames[n-1] + " de " + month[:4]
}

// Quote keeps the line breaks of a text in Markdown.
func Quote(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ") + "  "
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " ")
}

// IsVideo tells a video by its extension, to link it rather than show it.
func IsVideo(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".mov", ".m4v", ".webm", ".3gp", ".mkv":
		return true
	}
	return false
}

// IsImage is true for the files a Markdown viewer can show inline.
func IsImage(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".bmp":
		return true
	}
	return false
}

// MediaLink is the Markdown for a local media file.
func MediaLink(link string) string {
	switch {
	case IsImage(link):
		return fmt.Sprintf("![](%s)", link)
	case IsVideo(link):
		return fmt.Sprintf("[vídeo: %s](%s)", path.Base(link), link)
	default:
		return fmt.Sprintf("[fichero: %s](%s)", path.Base(link), link)
	}
}

// CopyMedia writes a file of the export to rel, once: media never changes.
// It returns false, without error, when the export does not have it.
func CopyMedia(ctx context.Context, run *plugin.Run, fsys fs.FS, name, rel string, modTime time.Time) (bool, error) {
	info, err := fs.Stat(fsys, name)
	if err != nil || info.IsDir() {
		return false, nil
	}
	if modTime.IsZero() {
		modTime = info.ModTime()
	}
	_, err = run.Out.Write(ctx, plugin.File{Path: rel, ModTime: modTime, Size: info.Size(),
		Open: func() (io.ReadCloser, error) { return fsys.Open(name) }})
	return err == nil, err
}

// CSV renders a table with a header, as spreadsheets open it.
func CSV(header []string, rows [][]string) []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	w.Write(header)
	w.WriteAll(rows) // flushes; writing to memory does not fail
	return b.Bytes()
}

// CleanPath makes a path found inside an export safe to open in it.
func CleanPath(name string) (string, bool) {
	name = path.Clean(strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "/"))
	if name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, true
}

// Union looks a file up in several file systems in turn: a big Meta export
// comes in several ZIPs, and a message in one part may point to a photo in
// another.
type Union []fs.FS

// Open implements fs.FS.
func (u Union) Open(name string) (fs.File, error) {
	for _, fsys := range u {
		if f, err := fsys.Open(name); err == nil {
			return f, nil
		}
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// Stat implements fs.StatFS.
func (u Union) Stat(name string) (fs.FileInfo, error) {
	for _, fsys := range u {
		if info, err := fs.Stat(fsys, name); err == nil {
			return info, nil
		}
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}
