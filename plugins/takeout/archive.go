package takeout

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	zipPart = iota
	tgzPart
	dirPart
)

// part is one piece of an export: a ZIP, a .tgz or an unpacked folder.
type part struct {
	path  string
	name  string
	key   string // changes when the file changes
	kind  int
	group string // the export it belongs to: «takeout-20250101T000000Z»
	seq   int    // 1 for «-001», 0 when the name has no number
}

// partName matches «takeout-20250101T101010Z-001.zip» and the like: Google
// numbers the parts of one export with three digits.
var partName = regexp.MustCompile(`(?i)^(.+)-(\d{3})(\.zip|\.tgz|\.tar\.gz)?$`)

func archiveKind(name string) int {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return zipPart
	case strings.HasSuffix(lower, ".tgz"), strings.HasSuffix(lower, ".tar.gz"):
		return tgzPart
	}
	return -1
}

func newPart(full string, kind int, info os.FileInfo) *part {
	name := filepath.Base(full)
	p := &part{path: full, name: name, kind: kind, group: name}
	p.key = fmt.Sprintf("%s|%d|%d", name, info.Size(), info.ModTime().Unix())
	if m := partName.FindStringSubmatch(name); m != nil {
		p.group = m[1]
		p.seq, _ = strconv.Atoi(m[2])
	}
	return p
}

// takeoutDir returns the «Takeout» folder of an unpacked export, or "".
func takeoutDir(dir string) string {
	if strings.EqualFold(filepath.Base(dir), "Takeout") {
		return dir
	}
	inner := filepath.Join(dir, "Takeout")
	if info, err := os.Stat(inner); err == nil && info.IsDir() {
		return inner
	}
	return ""
}

// dirKey sums up an unpacked export so that a change in it is noticed.
func dirKey(name, dir string) string {
	count, size, latest := 0, int64(0), int64(0)
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			count++
			size += info.Size()
			latest = max(latest, info.ModTime().Unix())
		}
		return nil
	})
	return fmt.Sprintf("%s|dir|%d|%d|%d", name, count, size, latest)
}

// findParts lists the parts under root: root itself if it is an archive or an
// unpacked export, or the archives and unpacked exports inside it.
func findParts(root string) ([]*part, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("no encuentro %s", root)
	}
	if !info.IsDir() {
		kind := archiveKind(root)
		if kind < 0 {
			return nil, nil
		}
		return []*part{newPart(root, kind, info)}, nil
	}
	if dir := takeoutDir(root); dir != "" {
		name := filepath.Base(root)
		return []*part{{path: dir, name: name, key: dirKey(name, dir), kind: dirPart, group: name}}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := []*part{}
	for _, entry := range entries {
		full := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			if dir := takeoutDir(full); dir != "" {
				p := &part{path: dir, name: entry.Name(), key: dirKey(entry.Name(), dir), kind: dirPart, group: entry.Name()}
				if m := partName.FindStringSubmatch(entry.Name()); m != nil {
					p.group = m[1]
					p.seq, _ = strconv.Atoi(m[2])
				}
				out = append(out, p)
			}
			continue
		}
		kind := archiveKind(entry.Name())
		if kind < 0 {
			continue
		}
		if i, err := entry.Info(); err == nil {
			out = append(out, newPart(full, kind, i))
		}
	}
	return out, nil
}

// groupParts puts together the parts of each export, the oldest export first
// (Google names them by date), so that what a newer one changed wins.
func groupParts(parts []*part) [][]*part {
	byGroup := map[string][]*part{}
	for _, p := range parts {
		byGroup[p.group] = append(byGroup[p.group], p)
	}
	names := make([]string, 0, len(byGroup))
	for name, list := range byGroup {
		names = append(names, name)
		sort.Slice(list, func(i, j int) bool {
			if list[i].seq != list[j].seq {
				return list[i].seq < list[j].seq
			}
			return list[i].name < list[j].name
		})
	}
	sort.Strings(names)
	out := make([][]*part, len(names))
	for i, name := range names {
		out[i] = byGroup[name]
	}
	return out
}

// missingPart returns the number of a part missing between the ones there
// are («002» when there are 001 and 003), or "". A photo and its JSON may be
// in different parts, so an export is read with all its parts together.
func missingPart(group []*part) string {
	want := 1
	for _, p := range group {
		if p.seq == 0 {
			continue
		}
		if p.seq > want {
			return fmt.Sprintf("%03d", want)
		}
		want = p.seq + 1
	}
	return ""
}

// looksLikeTakeout checks that an archive has a Takeout folder, reading only
// its list of files (or the first ones of a .tgz).
func looksLikeTakeout(p *part) (bool, error) {
	switch p.kind {
	case zipPart:
		r, err := zip.OpenReader(p.path)
		if err != nil {
			return false, err
		}
		defer r.Close()
		for _, f := range r.File {
			if _, ok := takeoutPrefix(f.Name); ok {
				return true, nil
			}
		}
		return false, nil
	case tgzPart:
		found, seen := false, 0
		err := walkTar(p.path, func(h *tar.Header, r io.Reader) error {
			if _, ok := takeoutPrefix(h.Name); ok {
				found = true
			}
			if seen++; found || seen > 50 {
				return errStop
			}
			return nil
		})
		if err != nil && err != errStop {
			return false, err
		}
		return found, nil
	}
	return true, nil
}

// errStop ends a walk of a .tgz early.
var errStop = errors.New("stop")

// takeoutPrefix strips «Takeout/» from a member name.
func takeoutPrefix(name string) (string, bool) {
	name = strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./")
	first, rest, found := strings.Cut(name, "/")
	if found && strings.EqualFold(first, "Takeout") {
		return rest, true
	}
	return name, false
}

// cleanMember returns the name of a member relative to the Takeout folder,
// or false for folders, macOS leftovers and names that try to leave it.
func cleanMember(name string) (string, bool, bool) {
	rel, prefixed := takeoutPrefix(name)
	if rel == "" || strings.HasSuffix(rel, "/") || strings.HasPrefix(rel, "/") {
		return "", prefixed, false
	}
	clean := path.Clean(rel)
	if clean != rel || clean == "." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "__MACOSX/") {
		return "", prefixed, false
	}
	return clean, prefixed, true
}

// entry is one file of an export.
type entry struct {
	name    string // relative to the Takeout folder: «Google Fotos/Viaje/IMG_1.jpg»
	size    int64
	modTime time.Time
	zf      *zip.File
	file    string // on disk: a file of an unpacked export, or a copy of a .tgz member
}

func (e *entry) open() (io.ReadCloser, error) {
	if e.zf != nil {
		return e.zf.Open()
	}
	if e.file != "" {
		return os.Open(e.file)
	}
	return nil, fmt.Errorf("%s no está disponible", e.name)
}

// readSmall reads a member that is small by nature (a photo's JSON, a note).
func (e *entry) readSmall(limit int64) ([]byte, error) {
	if e.size > limit {
		return nil, fmt.Errorf("%s es demasiado grande (%d bytes)", e.name, e.size)
	}
	r, err := e.open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, limit))
}

// top is the product folder of an entry, "" for files at the top.
func (e *entry) top() (string, string) {
	first, rest, found := strings.Cut(e.name, "/")
	if !found {
		return "", e.name
	}
	return first, rest
}

// index is the list of files of all the parts of an export. ZIP and folders
// are read in any order straight from the disk; .tgz can only be read from the
// start, so its small JSON files (the ones looked up from others, like the
// JSON of a photo) are copied to a temporary folder on a first pass.
type index struct {
	tmp     string
	entries map[string]*entry
	dirs    map[string]map[string]*entry // folder → file name → entry
	parts   map[*part][]*entry
	hints   map[string]int // product folder → what its files look like
	takeout bool
	closers []io.Closer
	copies  int
}

func newIndex(tmp string) *index {
	return &index{tmp: tmp, entries: map[string]*entry{}, dirs: map[string]map[string]*entry{}, parts: map[*part][]*entry{}, hints: map[string]int{}}
}

func (x *index) close() {
	for _, c := range x.closers {
		c.Close()
	}
}

func (x *index) add(p *part, e *entry) {
	x.entries[e.name] = e
	dir, base := path.Split(e.name)
	dir = strings.TrimSuffix(dir, "/")
	if x.dirs[dir] == nil {
		x.dirs[dir] = map[string]*entry{}
	}
	x.dirs[dir][base] = e
	x.parts[p] = append(x.parts[p], e)
	if top, rest := e.top(); top != "" {
		x.hints[top] |= hintsOf(rest)
	}
}

// smallJSON is the biggest JSON copied from a .tgz on the first pass.
const smallJSON = 1 << 20

func (x *index) scan(ctx context.Context, p *part) error {
	switch p.kind {
	case zipPart:
		r, err := zip.OpenReader(p.path)
		if err != nil {
			return err
		}
		x.closers = append(x.closers, r)
		for _, f := range r.File {
			name, prefixed, ok := cleanMember(f.Name)
			x.takeout = x.takeout || prefixed
			if !ok || f.FileInfo().IsDir() {
				continue
			}
			x.add(p, &entry{name: name, size: int64(f.UncompressedSize64), modTime: f.Modified, zf: f})
		}
		return nil
	case dirPart:
		x.takeout = true
		return filepath.WalkDir(p.path, func(full string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(p.path, full)
			if err != nil {
				return err
			}
			name, _, ok := cleanMember(filepath.ToSlash(rel))
			info, err := d.Info()
			if !ok || err != nil {
				return nil
			}
			x.add(p, &entry{name: name, size: info.Size(), modTime: info.ModTime(), file: full})
			return nil
		})
	case tgzPart:
		return walkTar(p.path, func(h *tar.Header, r io.Reader) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			name, prefixed, ok := cleanMember(h.Name)
			x.takeout = x.takeout || prefixed
			if !ok || h.Typeflag != tar.TypeReg {
				return nil
			}
			e := &entry{name: name, size: h.Size, modTime: h.ModTime}
			if strings.EqualFold(path.Ext(name), ".json") && h.Size <= smallJSON {
				file, err := x.copy(r)
				if err != nil {
					return err
				}
				e.file = file
			}
			x.add(p, e)
			return nil
		})
	}
	return nil
}

// copy writes a member to a temporary file.
func (x *index) copy(r io.Reader) (string, error) {
	x.copies++
	name := filepath.Join(x.tmp, fmt.Sprintf("m%07d", x.copies))
	f, err := os.Create(name)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, r)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return name, err
}

// each calls fn for every file of a part, in the order of the archive.
func (x *index) each(ctx context.Context, p *part, fn func(*entry) error) error {
	if p.kind != tgzPart {
		for _, e := range x.parts[p] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := fn(e); err != nil {
				return err
			}
		}
		return nil
	}
	// A .tgz is read again from the start; the members are the same, in the
	// same order, as on the first pass. Each one not copied then is copied now
	// to a temporary file (the writer reads a file twice) and removed after.
	list := x.parts[p]
	i := 0
	return walkTar(p.path, func(h *tar.Header, r io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, _, ok := cleanMember(h.Name)
		if !ok || h.Typeflag != tar.TypeReg {
			return nil
		}
		if i >= len(list) || list[i].name != name {
			return fmt.Errorf("%s cambió mientras se leía", p.name)
		}
		e := list[i]
		i++
		if e.file != "" {
			return fn(e)
		}
		file, err := x.copy(r)
		if err != nil {
			os.Remove(file)
			return err
		}
		e.file = file
		err = fn(e)
		e.file = ""
		os.Remove(file)
		return err
	})
}

// walkTar reads a .tgz from the start.
func walkTar(file string, fn func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}
