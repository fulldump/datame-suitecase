// Package dav keeps a copy of calendars and contacts over CalDAV and CardDAV,
// the open protocols of iCloud, Nextcloud, Fastmail, Radicale, Baïkal and most
// servers that are not Google or Microsoft.
//
// It is one integration with two switches, not two integrations: the person
// thinks of «my iCloud» or «my Nextcloud», with one address, one user and one
// app password for both, and both protocols share discovery, sync and
// fetching. Each calendar and address book goes to its own folder with every
// object as the server gave it (.ics, .vcf: the durable format), everything
// merged in one file that any app imports at once, and a Markdown summary.
//
// Only what changed is asked for: with sync-collection (RFC 6578) and the
// token of the last run when the server supports it, comparing ETags
// otherwise, and the changed objects come in batches with multiget. What is
// deleted on the server stays in the NAS and is marked in the summary.
package dav

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/fulldump/datame-suitcase/plugin"
)

// batch is how many objects are asked for in one multiget: big enough to save
// round trips, small enough for address books full of photos.
const batch = 50

func init() { plugin.Register(Plugin{}) }

// Plugin is the CalDAV/CardDAV integration.
type Plugin struct{}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:  "dav",
		Title: "Calendarios y contactos (CalDAV y CardDAV)",
		Description: "Copia tus calendarios y contactos de iCloud, Nextcloud, Fastmail, Radicale, Baïkal y otros: " +
			"cada evento y contacto en su fichero .ics o .vcf, todos juntos para importarlos de una vez, y un resumen para leer.",
		Help: "Usa una contraseña de aplicación, no la normal de tu cuenta. " +
			"iCloud: créala en appleid.apple.com → Inicio de sesión y seguridad → Contraseñas específicas de apps; el usuario es tu Apple ID (el correo). " +
			"Fastmail: Ajustes → Privacidad y seguridad → Contraseñas de aplicación, con acceso a CalDAV y CardDAV. " +
			"Nextcloud: Ajustes personales → Seguridad → Crear nueva contraseña de aplicación; el servidor es https://tu-servidor/remote.php/dav. " +
			"Radicale, Baïkal y otros: la dirección del servidor; los calendarios se buscan solos. " +
			"Google no funciona todavía: su CalDAV y CardDAV solo dejan entrar con OAuth, no con contraseñas de aplicación. " +
			"Para Google Calendar usa su «dirección secreta en formato iCal» con «Fichero de una dirección web», y para los contactos, Google Takeout. " +
			"Outlook no tiene CalDAV: hace falta un intermediario como DavMail, y poner aquí su dirección.",
		DefaultFolder:     "/Archivo/Agenda",
		DefaultEveryHours: 12,
		Fields: []plugin.Field{
			{Name: "server", Label: "Servidor", Type: "text", Required: true, Placeholder: "icloud, fastmail o https://tu-nextcloud.com/remote.php/dav",
				Help: "Escribe icloud o fastmail, o la dirección de tu servidor."},
			{Name: "user", Label: "Usuario", Type: "text", Required: true, Placeholder: "ana@example.org", Help: "Normalmente, tu correo."},
			{Name: "password", Label: "Contraseña de aplicación", Type: "password", Secret: true, Required: true},
			{Name: "calendars", Label: "Copiar los calendarios", Type: "checkbox", Default: "true"},
			{Name: "contacts", Label: "Copiar los contactos", Type: "checkbox", Default: "true"},
		},
	}
}

// settings is the configuration, read once.
type settings struct {
	urls     map[string]*url.URL // kind name → where to start looking
	kinds    []kind
	user     string
	password string
	google   bool
}

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func readSettings(config plugin.Config) (settings, error) {
	s := settings{urls: map[string]*url.URL{}, user: config.Get("user"), password: config.Get("password")}
	if config.Get("calendars") != "false" {
		s.kinds = append(s.kinds, calendars)
	}
	if config.Get("contacts") != "false" {
		s.kinds = append(s.kinds, contacts)
	}
	if len(s.kinds) == 0 {
		return s, errors.New("marca al menos una cosa que copiar: los calendarios o los contactos")
	}
	server := config.Get("server")
	switch strings.ToLower(server) {
	case "icloud", "apple":
		s.urls[calendars.name] = mustParse("https://caldav.icloud.com/")
		s.urls[contacts.name] = mustParse("https://contacts.icloud.com/")
	case "fastmail":
		s.urls[calendars.name] = mustParse("https://caldav.fastmail.com/dav/")
		s.urls[contacts.name] = mustParse("https://carddav.fastmail.com/dav/")
	case "google", "gmail":
		// Kept so that the person gets a clear answer from Google itself, in
		// case it ever accepts app passwords again.
		s.google = true
		s.urls[calendars.name] = mustParse("https://apidata.googleusercontent.com/caldav/v2/" + url.PathEscape(s.user) + "/user")
		s.urls[contacts.name] = mustParse("https://www.googleapis.com/carddav/v1/principals/" + url.PathEscape(s.user) + "/")
	case "nextcloud":
		return s, errors.New("escribe la dirección de tu Nextcloud: https://tu-servidor/remote.php/dav")
	case "outlook", "hotmail", "office365", "microsoft":
		return s, errors.New("Outlook no tiene CalDAV ni CardDAV: hace falta un intermediario como DavMail, y poner aquí su dirección")
	default:
		u, err := url.Parse(server)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return s, errors.New("el servidor debe ser icloud, fastmail o una dirección que empiece por https://")
		}
		u.User = nil // the password goes in its own field, never in the address
		if strings.HasSuffix(u.Host, "icloud.com") {
			s.urls[calendars.name] = mustParse("https://caldav.icloud.com/")
			s.urls[contacts.name] = mustParse("https://contacts.icloud.com/")
		} else {
			s.urls[calendars.name], s.urls[contacts.name] = u, u
		}
	}
	return s, nil
}

// Validate implements plugin.Plugin: finds the collections, which checks the
// address, the user and the password.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error {
	s, err := readSettings(env.Config)
	if err != nil {
		return err
	}
	for _, k := range s.kinds {
		c := newClient(env.HTTP, s, s.urls[k.name])
		if _, err := c.home(ctx, k, s.urls[k.name]); err != nil {
			return err
		}
	}
	return nil
}

// object is one event or contact as known from the last runs.
type object struct {
	File    string `json:"file"`              // name inside the folder of single objects
	ETag    string `json:"etag,omitempty"`    // the version of it that is in the NAS
	Deleted string `json:"deleted,omitempty"` // the day it was found deleted on the server
}

// collection is the state of one calendar or address book. The same, without
// the tokens, is written beside it as indice.json, so that a new install that
// points to the same folder knows what was deleted and what it already has.
type collection struct {
	Kind      string             `json:"kind"`
	Name      string             `json:"name"`
	URL       string             `json:"url"`
	Path      string             `json:"path"`
	Folder    string             `json:"folder"`
	SyncToken string             `json:"sync_token,omitempty"`
	CTag      string             `json:"ctag,omitempty"`
	Objects   map[string]*object `json:"objects"` // path on the server → object
}

type cursor struct {
	Collections map[string]*collection `json:"collections"` // kind and path → state
}

// index is the readable record written as indice.json in each collection.
type index struct {
	About   string             `json:"about"`
	Kind    string             `json:"kind"`
	Name    string             `json:"name"`
	URL     string             `json:"url"`
	Path    string             `json:"path"`
	Objects map[string]*object `json:"objects"`
}

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	s, err := readSettings(run.Config)
	if err != nil {
		return err
	}
	var cur cursor
	if err := run.Load(&cur); err != nil {
		return err
	}
	if cur.Collections == nil {
		cur.Collections = map[string]*collection{}
	}
	var problems []string
	for _, k := range s.kinds {
		c := newClient(run.HTTP, s, s.urls[k.name])
		home, err := c.home(ctx, k, s.urls[k.name])
		if err != nil {
			return err
		}
		list, err := c.collections(ctx, k, home)
		if err != nil {
			return err
		}
		run.Logf("%s: %d", k.folder, len(list))
		for _, rc := range list {
			if err := syncCollection(ctx, run, c, k, &cur, rc); err != nil {
				if ctx.Err() != nil || isAuth(err) {
					return err
				}
				// One broken calendar must not stop the others.
				run.Logf("%s: %s", rc.name, err)
				problems = append(problems, rc.name)
			}
			if err := run.Save(cur); err != nil {
				return err
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("no se pudo copiar: %s (mira el registro)", strings.Join(problems, ", "))
	}
	return nil
}

func syncCollection(ctx context.Context, run *plugin.Run, c *client, k kind, cur *cursor, rc remote) error {
	key := k.name + " " + rc.path
	col := cur.Collections[key]
	fresh := col == nil
	if fresh {
		var err error
		if col, err = adopt(ctx, run, k, cur, rc); err != nil {
			return err
		}
		cur.Collections[key] = col
	}
	col.Name, col.URL = rc.name, rc.url.String()

	// The listing already says whether anything moved: nothing more to ask.
	if !fresh && rc.syncToken != "" && rc.syncToken == col.SyncToken {
		return nil
	}
	if !fresh && !rc.supportsSync && rc.ctag != "" && rc.ctag == col.CTag {
		return nil
	}

	ch, err := changesOf(ctx, c, rc, col)
	if err != nil {
		return err
	}
	var fetch []string
	for p, etag := range ch.changed {
		if o := col.Objects[p]; o != nil && o.Deleted == "" && etag != "" && o.ETag == etag {
			continue
		}
		fetch = append(fetch, p)
	}
	sort.Strings(fetch)
	gone := ch.gone
	if ch.full {
		for p, o := range col.Objects {
			if _, there := ch.changed[p]; !there && o.Deleted == "" {
				gone = append(gone, p)
			}
		}
	}

	dirty := fresh
	for start := 0; start < len(fetch); start += batch {
		paths := fetch[start:min(start+batch, len(fetch))]
		got, err := c.multiget(ctx, k, rc, paths)
		if err != nil {
			return err
		}
		for _, p := range paths {
			f, ok := got[p]
			if !ok {
				continue
			}
			o := col.Objects[p]
			first := o == nil
			if first {
				o = &object{File: objectFile(col, k, p)}
				col.Objects[p] = o
			}
			if _, err := run.Out.Write(ctx, plugin.File{Path: col.Folder + "/" + k.sub + "/" + o.File, ModTime: run.Now(), Data: f.data, Mutable: true}); err != nil {
				return err
			}
			cacheStore(run, col, o, f.data)
			o.ETag = f.etag
			if o.ETag == "" {
				o.ETag = ch.changed[p]
			}
			o.Deleted = ""
			if first {
				run.Item(1)
			}
			dirty = true
		}
	}
	sort.Strings(gone)
	for _, p := range gone {
		if o := col.Objects[p]; o != nil && o.Deleted == "" {
			o.Deleted = run.Now().Format("2006-01-02")
			run.Logf("%s: %s se borró en el servidor; se queda en la copia", col.Name, o.File)
			dirty = true
		}
	}
	if dirty {
		if err := rebuild(ctx, run, k, col); err != nil {
			return err
		}
	}
	// Only now, with everything written, the server may forget what it told.
	col.SyncToken, col.CTag = ch.token, rc.ctag
	return nil
}

// changesOf asks with sync-collection when the server has it, and falls back
// to comparing ETags when it does not or refuses the report.
func changesOf(ctx context.Context, c *client, rc remote, col *collection) (*changes, error) {
	if rc.supportsSync || rc.syncToken != "" || col.SyncToken != "" {
		ch, err := c.syncCollection(ctx, rc, col.SyncToken)
		if err != nil && col.SyncToken != "" && hasStatus(err, 400, 403, 409, 410, 412) {
			// The token expired (servers keep them for a while): list it all again.
			ch, err = c.syncCollection(ctx, rc, "")
		}
		if err == nil {
			return ch, nil
		}
		if isAuth(err) || ctx.Err() != nil || !hasStatus(err, 400, 403, 404, 405, 409, 415, 422, 501) {
			return nil, err
		}
	}
	ch, err := c.etags(ctx, rc)
	if err != nil {
		return nil, err
	}
	ch.token = ""
	return ch, nil
}

// adopt starts the state of a collection not seen before: it chooses its
// folder by its name, and if that folder has an indice.json of this same
// collection (the state was lost, or a new install), goes on from it.
func adopt(ctx context.Context, run *plugin.Run, k kind, cur *cursor, rc remote) (*collection, error) {
	used := map[string]bool{}
	for _, other := range cur.Collections {
		used[other.Folder] = true
	}
	base := plugin.SafeName(rc.name, 80)
	for n := 1; ; n++ {
		folder := k.folder + "/" + base
		if n > 1 {
			folder += "-" + strconv.Itoa(n)
		}
		if used[folder] {
			continue
		}
		col := &collection{Kind: k.name, Path: rc.path, Folder: folder, Objects: map[string]*object{}}
		data, err := run.Out.Read(ctx, folder+"/indice.json")
		if errors.Is(err, plugin.ErrNotExist) {
			return col, nil
		}
		if err != nil {
			return nil, err
		}
		var idx index
		if json.Unmarshal(data, &idx) != nil || idx.Kind != k.name || idx.Path != rc.path {
			continue // the folder of another collection with the same name
		}
		if idx.Objects != nil {
			col.Objects = idx.Objects
		}
		run.Logf("%s: sigue con lo que ya había en %s", rc.name, folder)
		return col, nil
	}
}

// objectFile names the file of an object after its name on the server, which
// is stable (usually its UID), keeping it unique in the collection.
func objectFile(col *collection, k kind, p string) string {
	name := plugin.SafeName(lastSegment(p), 120)
	if strings.EqualFold(path.Ext(name), k.ext) {
		name = name[:len(name)-len(k.ext)]
	}
	name += k.ext
	taken := false
	for _, o := range col.Objects {
		if strings.EqualFold(o.File, name) {
			taken = true
			break
		}
	}
	if taken {
		name = strings.TrimSuffix(name, k.ext) + "-" + plugin.ShortHash(p) + k.ext
	}
	return name
}

// ---- Local working copy ----

// The merged file and the summary need every object, not only the changed
// ones. They are kept in the cache folder; if it is lost, they are read back
// from the NAS.

func cachePath(run *plugin.Run, col *collection, o *object) string {
	if run.CacheDir == "" {
		return ""
	}
	return filepath.Join(run.CacheDir, "dav", plugin.ShortHash(col.Kind+" "+col.Path), o.File)
}

func cacheStore(run *plugin.Run, col *collection, o *object, data []byte) {
	file := cachePath(run, col, o)
	if file == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(file), 0o700) == nil {
		os.WriteFile(file, data, 0o600) // only a shortcut: the NAS has it
	}
}

func load(ctx context.Context, run *plugin.Run, k kind, col *collection, o *object) ([]byte, error) {
	if file := cachePath(run, col, o); file != "" {
		if data, err := os.ReadFile(file); err == nil {
			return data, nil
		}
	}
	data, err := run.Out.Read(ctx, col.Folder+"/"+k.sub+"/"+o.File)
	if err == nil {
		cacheStore(run, col, o, data)
	}
	return data, err
}

// rebuild writes the merged file, the summary and the index of a collection.
func rebuild(ctx context.Context, run *plugin.Run, k kind, col *collection) error {
	paths := make([]string, 0, len(col.Objects))
	for p := range col.Objects {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool { return col.Objects[paths[i]].File < col.Objects[paths[j]].File })
	var items []stored
	for _, p := range paths {
		o := col.Objects[p]
		data, err := load(ctx, run, k, col, o)
		if errors.Is(err, plugin.ErrNotExist) {
			run.Logf("%s: falta %s en el NAS", col.Name, o.File)
			continue
		}
		if err != nil {
			return err
		}
		items = append(items, stored{file: k.sub + "/" + o.File, deleted: o.Deleted, data: data})
	}
	base := path.Base(col.Folder)
	var merged, summary []byte
	if k.name == calendars.name {
		merged, summary = mergeCalendar(col.Name, items), calendarSummary(col, items)
	} else {
		merged, summary = mergeContacts(items), contactsSummary(col, items)
	}
	what := "calendario"
	if k.name == contacts.name {
		what = "libro de contactos"
	}
	idx := index{About: "Lo copiado de este " + what + ": cada elemento del servidor con su fichero y, si se borró en el servidor, el día en que se vio borrado.",
		Kind: col.Kind, Name: col.Name, URL: col.URL, Path: col.Path, Objects: col.Objects}
	for _, f := range []plugin.File{
		{Path: col.Folder + "/" + base + k.ext, Data: merged},
		{Path: col.Folder + "/" + base + ".md", Data: summary},
		{Path: col.Folder + "/indice.json", Data: plugin.JSON(idx)},
	} {
		f.ModTime, f.Mutable = run.Now(), true
		if _, err := run.Out.Write(ctx, f); err != nil {
			return err
		}
	}
	return nil
}
