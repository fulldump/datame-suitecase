package takeout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path"
	"strings"
	"unicode"

	"github.com/fulldump/datame-suitcase/plugin"
)

// target is where a product folder of the export goes.
type target struct {
	key string // product key, for the checkboxes
	out string // folder in the NAS
}

// folderNames are the product folders Takeout writes, in English and Spanish,
// lower case. Google translates them to the language of the account.
var folderNames = map[string]target{
	"google photos": {"fotos", "Fotos"}, "google fotos": {"fotos", "Fotos"}, "photos": {"fotos", "Fotos"}, "fotos": {"fotos", "Fotos"},
	"drive": {"drive", "Drive"}, "google drive": {"drive", "Drive"},
	"mail": {"correo", "Correo"}, "correo": {"correo", "Correo"}, "gmail": {"correo", "Correo"},
	"keep": {"keep", "Keep"}, "google keep": {"keep", "Keep"},
	"calendar": {"calendario", "Calendario"}, "calendario": {"calendario", "Calendario"}, "google calendar": {"calendario", "Calendario"},
	"contacts": {"contactos", "Contactos"}, "contactos": {"contactos", "Contactos"}, "google contacts": {"contactos", "Contactos"},
	"youtube and youtube music": {"youtube", "YouTube"}, "youtube y youtube music": {"youtube", "YouTube"}, "youtube": {"youtube", "YouTube"},
	"maps (your places)": {"maps", "Maps/Sitios"}, "maps (tus sitios)": {"maps", "Maps/Sitios"},
	"saved": {"maps", "Maps/Guardado"}, "guardado": {"maps", "Maps/Guardado"}, "guardados": {"maps", "Maps/Guardado"},
	"maps": {"maps", "Maps/Maps"}, "google maps": {"maps", "Maps/Maps"},
	"location history": {"ubicaciones", "Ubicaciones"}, "location history (timeline)": {"ubicaciones", "Ubicaciones"},
	"historial de ubicaciones": {"ubicaciones", "Ubicaciones"}, "historial de ubicaciones (timeline)": {"ubicaciones", "Ubicaciones"},
	"historial de ubicaciones (cronología)": {"ubicaciones", "Ubicaciones"}, "timeline": {"ubicaciones", "Ubicaciones"}, "cronología": {"ubicaciones", "Ubicaciones"},
	"chrome": {"chrome", "Chrome"},
}

// Hints from the files of a product folder, for names not in the list (other
// languages, renamed products).
const (
	hintMbox = 1 << iota
	hintICS
	hintVCF
	hintPhotos
	hintLocation
)

func hintsOf(rel string) int {
	lower := strings.ToLower(rel)
	first, _, _ := strings.Cut(rel, "/")
	switch {
	case strings.HasSuffix(lower, ".mbox"):
		return hintMbox
	case strings.HasSuffix(lower, ".ics"):
		return hintICS
	case strings.HasSuffix(lower, ".vcf"):
		return hintVCF
	case strings.HasSuffix(lower, ".supplemental-metadata.json"), yearFolder.MatchString(first):
		return hintPhotos
	case path.Base(lower) == "records.json", strings.HasPrefix(lower, "semantic location history/"):
		return hintLocation
	}
	return 0
}

func classify(top string, hints int) target {
	if top == "" {
		return target{"otros", "Otros/Takeout"}
	}
	if t, ok := folderNames[strings.ToLower(strings.TrimSpace(top))]; ok {
		return t
	}
	switch {
	case hints&hintMbox != 0:
		return target{"correo", "Correo"}
	case hints&hintPhotos != 0:
		return target{"fotos", "Fotos"}
	case hints&hintLocation != 0:
		return target{"ubicaciones", "Ubicaciones"}
	case hints&hintICS != 0:
		return target{"calendario", "Calendario"}
	case hints&hintVCF != 0:
		return target{"contactos", "Contactos"}
	}
	return target{"otros", "Otros/" + cleanName(top)}
}

// importer writes the files of one export.
type importer struct {
	run *plugin.Run
	c   *cursor
	idx *index
	tmp string

	taken    map[string]string          // Fotos path → content hash, to tell apart photos with the same name
	albums   map[string]map[string]bool // album folder of the export → photo paths
	jsons    map[string]*entry          // JSON files met in photo folders, sidecar or not
	sidecars map[string]bool            // JSON files used as the sidecar of a photo
	written  map[string]bool            // sidecars written in this run
	counts   map[string]int             // what was new, for the log
	pending  int
}

func newImporter(run *plugin.Run, c *cursor, idx *index, tmp string) *importer {
	x := &importer{run: run, c: c, idx: idx, tmp: tmp,
		taken: map[string]string{}, albums: map[string]map[string]bool{}, jsons: map[string]*entry{},
		sidecars: map[string]bool{}, written: map[string]bool{}, counts: map[string]int{}}
	for hash, p := range c.Photos {
		x.taken[p] = hash
	}
	return x
}

// created counts a new item, for the run and for the log.
func (x *importer) created(what string, result plugin.Result) {
	if result == plugin.Created {
		x.run.Item(1)
		x.counts[what]++
	}
}

func (x *importer) handle(ctx context.Context, e *entry) error {
	top, rest := e.top()
	t := classify(top, x.idx.hints[top])
	if !enabled(x.run.Config, t.key) {
		return nil
	}
	lower := strings.ToLower(rest)
	var err error
	switch {
	case t.key == "fotos":
		err = x.photo(ctx, e, rest)
	case t.key == "correo" && strings.HasSuffix(lower, ".mbox"):
		err = x.mbox(ctx, e, rest)
	case t.key == "keep":
		err = x.keep(ctx, e, rest)
	case t.key == "calendario" && strings.HasSuffix(lower, ".ics"):
		err = x.calendar(ctx, e, t.out+"/"+cleanPath(rest))
	case t.key == "contactos" && strings.HasSuffix(lower, ".vcf"):
		err = x.contacts(ctx, e, t.out+"/"+cleanPath(rest))
	case t.key == "ubicaciones" && path.Base(lower) == "records.json":
		err = x.records(ctx, e, t.out+"/"+cleanPath(rest))
	default:
		_, err = x.raw(ctx, e, t.out+"/"+cleanPath(rest), t.out)
	}
	if err != nil {
		return err
	}
	// Save the cursor now and then, so a long import cut half way does not
	// look at every photo again to know where it is.
	if x.pending++; x.pending%500 == 0 {
		return x.run.Save(*x.c)
	}
	return nil
}

// raw copies a file as it comes. Files in a snapshot may change from one
// export to the next (a document edited in Drive), so they are mutable: the
// NAS keeps the version before. what names it in the log; "" does not count.
func (x *importer) raw(ctx context.Context, e *entry, out, what string) (plugin.Result, error) {
	result, err := x.run.Out.Write(ctx, plugin.File{Path: out, ModTime: e.modTime, Size: e.size, Open: e.open, Mutable: true})
	if err == nil && what != "" {
		x.created(what, result)
	}
	return result, err
}

func (x *importer) finish(ctx context.Context) error {
	return x.finishPhotos(ctx)
}

// cleanName keeps a name as the person gave it (accents, spaces: they are
// their own folders and files) and only takes out what some file systems do
// not allow.
func cleanName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 32 || r == 127 || strings.ContainsRune(`/\:*?"<>|`, r) || !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
	}
	out := strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	if len(out) > 150 {
		// Keep the extension, which tells what the file is, and cut on a
		// whole character.
		ext := path.Ext(out)
		if len(ext) > 10 {
			ext = ""
		}
		cut := 150 - len(ext)
		for cut > 0 && !isRuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + ext
	}
	if out == "" || out == "." || out == ".." {
		return "_"
	}
	return out
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// cleanPath applies cleanName to each folder of a path.
func cleanPath(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = cleanName(p)
	}
	return strings.Join(parts, "/")
}

// contentHash is the SHA-256 of a member, read as a stream.
func contentHash(e *entry) (string, error) {
	r, err := e.open()
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hash16 is 16 hex characters (64 bits) of the SHA-256 of a text: enough not
// to mix up the messages or photos of a lifetime.
func hash16(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}
