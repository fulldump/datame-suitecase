// Package bluesky keeps a copy of a Bluesky (AT Protocol) account.
//
// An AT Protocol account is a signed repository of records, and the PDS that
// hosts it exports it whole as one CAR file (com.atproto.sync.getRepo). That
// file is the canonical backup: it holds every post, like, repost, follow and
// list, and each block is checked against its hash, so it is kept on the NAS
// exactly as it came. From it, the readable copy is rendered: posts by month
// in JSON and Markdown, likes, reposts, follows, lists and the profile, merged
// with what is already on the NAS so that a post deleted online stays in the
// copy. Images and videos (blobs) are downloaded once each, by CID.
//
// Later runs ask only for what changed since the last revision (getRepo with
// «since»), which gives a small CAR with the new blocks. Joined with the
// previous ones it makes the whole repository again, so the NAS keeps a full
// CAR and a chain of small ones after it; every so often a full one starts a
// new chain.
package bluesky

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

func init() { plugin.Register(Plugin{}) }

// maxChain is how many incremental CARs follow a full one before the next
// full download. It bounds how many files a restore has to join.
const maxChain = 30

// Plugin is the Bluesky integration.
type Plugin struct{}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:  "bluesky",
		Title: "Bluesky",
		Description: "Copia tu cuenta de Bluesky entera: el repositorio oficial (fichero CAR verificable), y para leer, tus publicaciones por meses en JSON y Markdown " +
			"con sus fotos y vídeos, «me gusta», republicaciones, seguidos, listas y perfil.",
		Help: "En Bluesky ve a Ajustes → Privacidad y seguridad → Contraseñas de aplicación → Añadir contraseña. Ponle un nombre (por ejemplo «Datame») " +
			"y no marques el acceso a mensajes directos. Copia la contraseña (xxxx-xxxx-xxxx-xxxx): no es la de tu cuenta y se puede borrar cuando quieras.",
		DefaultFolder:     "/Archivo/Bluesky",
		DefaultEveryHours: 6,
		Fields: []plugin.Field{
			{Name: "handle", Label: "Usuario", Type: "text", Required: true, Placeholder: "ana.bsky.social", Help: "Tu nombre de usuario, sin la @."},
			{Name: "password", Label: "Contraseña de aplicación", Type: "password", Required: true, Secret: true, Help: "Ajustes → Contraseñas de aplicación. No uses la contraseña de tu cuenta."},
			{Name: "pds", Label: "Servidor (PDS)", Type: "url", Placeholder: defaultEntryway, Help: "Déjalo vacío si tu cuenta es de bsky.social. Solo si tienes tu propio servidor."},
			{Name: "media", Label: "Descargar las fotos y vídeos", Type: "checkbox", Default: "true"},
		},
	}
}

// Validate implements plugin.Plugin: the app password must open a session.
func (p Plugin) Validate(ctx context.Context, env plugin.Env) error {
	if err := plugin.CheckRequired(p.Info(), env.Config); err != nil {
		return err
	}
	_, _, err := login(ctx, env.HTTP, env.Config.Get("pds"), env.Config.Get("handle"), env.Config.Get("password"))
	return err
}

type pendingBlob struct {
	Path string `json:"path,omitempty"` // empty: not used by any record, goes to media/otros
	Time int64  `json:"time,omitempty"` // date of the post, for the file
}

type cursor struct {
	DID      string                 `json:"did,omitempty"`
	Rev      string                 `json:"rev,omitempty"`       // revision rendered last
	Root     string                 `json:"root,omitempty"`      // its commit
	Chain    []string               `json:"chain,omitempty"`     // CARs on the NAS that make that revision: a full one, then increments
	BlobsRev string                 `json:"blobs_rev,omitempty"` // blobs listed up to this revision
	Pending  map[string]pendingBlob `json:"pending,omitempty"`   // blobs listed, not downloaded yet
}

// Sync implements plugin.Plugin.
func (p Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	var cur cursor
	if err := run.Load(&cur); err != nil {
		return err
	}
	c, s, err := login(ctx, run.HTTP, run.Config.Get("pds"), run.Config.Get("handle"), run.Config.Get("password"))
	if err != nil {
		return err
	}
	if cur.DID != "" && cur.DID != s.DID {
		run.Logf("La cuenta es otra (%s): se empieza de nuevo", s.DID)
		cur = cursor{}
	}
	cur.DID = s.DID
	cacheDir, cleanup, err := cacheFor(run.CacheDir, s.DID)
	if err != nil {
		return err
	}
	defer cleanup()

	var latest struct {
		CID string `json:"cid"`
		Rev string `json:"rev"`
	}
	if err := c.get(ctx, "com.atproto.sync.getLatestCommit", url.Values{"did": {s.DID}}, &latest); err != nil {
		return err
	}
	if latest.Rev != "" && latest.Rev == cur.Rev {
		run.Logf("El repositorio no ha cambiado (revisión %s)", cur.Rev)
		if cur.BlobsRev != cur.Rev && on(run.Config, "media") {
			// A previous run could not finish listing the blobs: the paths need
			// the records, which are in the CARs already saved.
			blocks := map[cid][]byte{}
			if err := loadChain(ctx, run, cacheDir, cur.Chain, blocks); err != nil {
				return err
			}
			root, err := parseCID(cur.Root)
			if err != nil {
				return err
			}
			r, err := parseRepo(blocks, root)
			if err != nil {
				return err
			}
			if err := listBlobs(ctx, run, c, &cur, blobIndex(r)); err != nil {
				return err
			}
		}
		return downloadBlobs(ctx, run, c, &cur, cacheDir)
	}

	r, err := fetchRepo(ctx, run, c, &cur, cacheDir, s)
	if err != nil {
		return err
	}
	if err := render(ctx, run, r, s.Handle); err != nil {
		return err
	}
	if err := run.Save(cur); err != nil {
		return err
	}
	if !on(run.Config, "media") {
		return nil
	}
	if err := listBlobs(ctx, run, c, &cur, blobIndex(r)); err != nil {
		return err
	}
	return downloadBlobs(ctx, run, c, &cur, cacheDir)
}

func on(config plugin.Config, name string) bool { return config.Get(name) != "false" }

// cacheFor is the local folder with copies of the CARs on the NAS, so joining
// an increment does not mean reading them all back. It is only a shortcut:
// when it is lost, they are read from the NAS.
func cacheFor(cacheDir, did string) (string, func(), error) {
	if cacheDir == "" {
		dir, err := os.MkdirTemp("", "bluesky-")
		return dir, func() { os.RemoveAll(dir) }, err
	}
	dir := filepath.Join(cacheDir, plugin.SafeName(did, 100))
	return dir, func() {}, os.MkdirAll(dir, 0o700)
}

// loadChain reads the CARs of the chain into blocks, from the cache or the NAS.
func loadChain(ctx context.Context, run *plugin.Run, cacheDir string, chain []string, blocks map[cid][]byte) error {
	if len(chain) == 0 {
		return errMissingBlock
	}
	for _, rel := range chain {
		local := filepath.Join(cacheDir, path.Base(rel))
		f, err := os.Open(local)
		if errors.Is(err, os.ErrNotExist) {
			data, err := run.Out.Read(ctx, rel)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if err := os.WriteFile(local, data, 0o600); err != nil {
				return err
			}
			if _, err := readCAR(bytes.NewReader(data), blocks); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			continue
		}
		if err != nil {
			return err
		}
		_, err = readCAR(f, blocks)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	return nil
}

type repo struct {
	commit
	records []record
}

func parseRepo(blocks map[cid][]byte, root cid) (*repo, error) {
	if root == "" {
		return nil, errMissingBlock
	}
	cm, err := readCommit(blocks, root)
	if err != nil {
		return nil, err
	}
	r := &repo{commit: cm}
	err = walkRecords(blocks, cm.Data, func(rec record) error {
		r.records = append(r.records, rec)
		return nil
	})
	return r, err
}

// fetchRepo downloads what changed, joins it with what was there, keeps the
// CAR on the NAS and moves the cursor's chain.
func fetchRepo(ctx context.Context, run *plugin.Run, c *client, cur *cursor, cacheDir string, s *session) (*repo, error) {
	since := ""
	if cur.Rev != "" && len(cur.Chain) > 0 && len(cur.Chain) < maxChain {
		since = cur.Rev
	}
	for {
		blocks := map[cid][]byte{}
		if since != "" {
			if err := loadChain(ctx, run, cacheDir, cur.Chain, blocks); err != nil {
				run.Logf("No se pueden leer las copias anteriores del repositorio (%s): se descarga entero", err)
				since = ""
				continue
			}
		}
		params := url.Values{"did": {s.DID}}
		if since != "" {
			params.Set("since", since)
			run.Logf("Descargando los cambios del repositorio desde la revisión %s", since)
		} else {
			run.Logf("Descargando el repositorio entero")
		}
		file, _, err := c.download(ctx, cacheDir, "com.atproto.sync.getRepo", params, maxCAR)
		if err != nil {
			return nil, err
		}
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		roots, err := readCAR(f, blocks)
		f.Close()
		if err == nil && len(roots) == 0 {
			err = errors.New("el CAR no tiene raíz")
		}
		var r *repo
		if err == nil {
			r, err = parseRepo(blocks, roots[0])
		}
		if err == nil && r.DID != s.DID {
			err = fmt.Errorf("el repositorio descargado es de %s, no de %s", r.DID, s.DID)
		}
		if err != nil {
			os.Remove(file)
			if since != "" && errors.Is(err, errMissingBlock) {
				run.Logf("Los cambios no encajan con la copia anterior: se descarga entero")
				since = ""
				continue
			}
			return nil, fmt.Errorf("el repositorio descargado no es válido: %w", err)
		}

		name := plugin.SafeName(r.Rev+"-completo.car", 120)
		if since != "" {
			name = plugin.SafeName(r.Rev+"-desde-"+since+".car", 120)
		}
		rel := "repositorio/" + name
		local := filepath.Join(cacheDir, name)
		if err := os.Rename(file, local); err != nil {
			return nil, err
		}
		info, err := os.Stat(local)
		if err != nil {
			return nil, err
		}
		if _, err := run.Out.Write(ctx, plugin.File{Path: rel, ModTime: run.Now(), Size: info.Size(),
			Open: func() (io.ReadCloser, error) { return os.Open(local) }}); err != nil {
			return nil, err
		}
		if since != "" {
			cur.Chain = append(cur.Chain, rel)
		} else {
			// A new full CAR: the old chain is not needed locally any more.
			for _, old := range cur.Chain {
				if path.Base(old) != name {
					os.Remove(filepath.Join(cacheDir, path.Base(old)))
				}
			}
			cur.Chain = []string{rel}
		}
		cur.Rev, cur.Root = r.Rev, roots[0].String()
		run.Logf("Repositorio en la revisión %s: %d registros", r.Rev, len(r.records))
		return r, nil
	}
}

// ---- Records ----

type blobRef struct {
	Type string `json:"$type"`
	Ref  struct {
		Link string `json:"$link"`
	} `json:"ref"`
	CID      string `json:"cid"` // the old form of a blob
	MimeType string `json:"mimeType"`
}

func (b *blobRef) id() string {
	if b == nil {
		return ""
	}
	if b.Ref.Link != "" {
		return b.Ref.Link
	}
	return b.CID
}

type strongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

type embed struct {
	Type   string `json:"$type"`
	Images []struct {
		Image *blobRef `json:"image"`
		Alt   string   `json:"alt"`
	} `json:"images"`
	Video    *blobRef `json:"video"`
	Alt      string   `json:"alt"`
	External *struct {
		URI   string `json:"uri"`
		Title string `json:"title"`
	} `json:"external"`
	Record *struct {
		URI    string     `json:"uri"`
		Record *strongRef `json:"record"`
	} `json:"record"`
	Media *embed `json:"media"`
}

type postRecord struct {
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
	Reply     *struct {
		Parent strongRef `json:"parent"`
	} `json:"reply"`
	Embed  *embed `json:"embed"`
	Facets []struct {
		Features []struct {
			Type string `json:"$type"`
			URI  string `json:"uri"`
		} `json:"features"`
	} `json:"facets"`
}

type subjectRecord struct {
	Subject   json.RawMessage `json:"subject"` // a strongRef (likes, reposts) or a DID (follows)
	CreatedAt string          `json:"createdAt"`
	List      string          `json:"list"`
	Name      string          `json:"name"`
}

// item is a record as saved in the JSON files.
type item struct {
	URI    string            `json:"uri"`
	CID    string            `json:"cid"`
	Record json.RawMessage   `json:"record"`
	Media  map[string]string `json:"local_media,omitempty"` // blob CID → file in the folder

	created time.Time
	rkey    string
}

func newItem(did string, rec record) *item {
	raw, _ := json.Marshal(toJSON(rec.Value))
	it := &item{URI: "at://" + did + "/" + rec.Collection + "/" + rec.RKey, CID: rec.CID.String(), Record: raw, rkey: rec.RKey}
	it.fill()
	return it
}

// fill derives the date and rkey, also for items read back from the NAS.
func (it *item) fill() {
	if it.rkey == "" {
		it.rkey = path.Base(it.URI)
	}
	var head struct {
		CreatedAt string `json:"createdAt"`
	}
	json.Unmarshal(it.Record, &head)
	if t, err := time.Parse(time.RFC3339Nano, head.CreatedAt); err == nil {
		it.created = t.UTC()
	} else {
		it.created = tidTime(it.rkey)
	}
}

const tidAlphabet = "234567abcdefghijklmnopqrstuvwxyz"

// tidTime reads the time inside a record key (a TID: microseconds since 1970
// in sortable base32), for records without createdAt.
func tidTime(rkey string) time.Time {
	if len(rkey) != 13 {
		return time.Time{}
	}
	var v uint64
	for _, r := range rkey {
		i := strings.IndexRune(tidAlphabet, r)
		if i < 0 {
			return time.Time{}
		}
		v = v<<5 | uint64(i)
	}
	return time.UnixMicro(int64(v >> 10)).UTC()
}

var extensions = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif", "image/heic": ".heic",
	"image/avif": ".avif", "video/mp4": ".mp4", "video/quicktime": ".mov", "video/webm": ".webm", "video/mpeg": ".mpeg",
}

func extension(mimeType string) string {
	mimeType, _, _ = strings.Cut(mimeType, ";")
	if ext, ok := extensions[strings.TrimSpace(strings.ToLower(mimeType))]; ok {
		return ext
	}
	return ".bin"
}

func monthOf(t time.Time) string {
	if t.IsZero() {
		return "0000-00"
	}
	return t.Format("2006-01")
}

func postMediaPath(id, mimeType string, created time.Time) string {
	m := monthOf(created)
	return fmt.Sprintf("media/%s/%s/%s", m[:4], m, plugin.SafeName(id+extension(mimeType), 120))
}

// blobIndex says where each blob of the repository goes on the NAS.
func blobIndex(r *repo) map[string]pendingBlob {
	index := map[string]pendingBlob{}
	for _, rec := range r.records {
		switch rec.Collection {
		case "app.bsky.feed.post":
			it := newItem(r.DID, rec)
			blobsOfJSON(toJSON(rec.Value), func(id, mimeType string) {
				index[id] = pendingBlob{Path: postMediaPath(id, mimeType, it.created), Time: it.created.Unix()}
			})
		case "app.bsky.actor.profile":
			blobsOfJSON(toJSON(rec.Value), func(id, mimeType string) {
				index[id] = pendingBlob{Path: "perfil/" + plugin.SafeName(id+extension(mimeType), 120)}
			})
		}
	}
	return index
}

// ---- Rendering ----

func render(ctx context.Context, run *plugin.Run, r *repo, handle string) error {
	byCollection := map[string][]*item{}
	var profile any
	for _, rec := range r.records {
		if rec.Collection == "app.bsky.actor.profile" && rec.RKey == "self" {
			profile = toJSON(rec.Value)
			continue
		}
		byCollection[rec.Collection] = append(byCollection[rec.Collection], newItem(r.DID, rec))
	}
	run.Logf("%d publicaciones, %d «me gusta», %d republicaciones, %d seguidos",
		len(byCollection["app.bsky.feed.post"]), len(byCollection["app.bsky.feed.like"]),
		len(byCollection["app.bsky.feed.repost"]), len(byCollection["app.bsky.graph.follow"]))

	if _, err := run.Out.Write(ctx, plugin.File{Path: "perfil.json", ModTime: run.Now(), Mutable: true,
		Data: plugin.JSON(map[string]any{"did": r.DID, "handle": handle, "profile": profile})}); err != nil {
		return err
	}
	if err := writePosts(ctx, run, r.DID, handle, byCollection["app.bsky.feed.post"]); err != nil {
		return err
	}
	if err := writeSubjects(ctx, run, "me-gusta", "«Me gusta» de @"+handle, byCollection["app.bsky.feed.like"]); err != nil {
		return err
	}
	if err := writeSubjects(ctx, run, "republicaciones", "Republicaciones de @"+handle, byCollection["app.bsky.feed.repost"]); err != nil {
		return err
	}
	if err := writeFollows(ctx, run, byCollection["app.bsky.graph.follow"]); err != nil {
		return err
	}
	return writeLists(ctx, run, handle, byCollection["app.bsky.graph.list"], byCollection["app.bsky.graph.listitem"])
}

func readItems(ctx context.Context, run *plugin.Run, rel string) (map[string]*item, error) {
	out := map[string]*item{}
	data, err := run.Out.Read(ctx, rel)
	if errors.Is(err, plugin.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*item
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s en el NAS no se puede leer: %w", rel, err)
	}
	for _, it := range list {
		if it.URI != "" {
			it.fill()
			out[it.URI] = it
		}
	}
	return out, nil
}

// merge adds the items of the repository to the ones on the NAS (which keeps
// what was deleted online) and returns them sorted, and how many are new.
func merge(existing map[string]*item, fresh []*item, newestFirst bool) ([]*item, int) {
	added := 0
	for _, it := range fresh {
		if old, ok := existing[it.URI]; !ok {
			added++
		} else if old.Media != nil && it.Media == nil {
			it.Media = old.Media
		}
		existing[it.URI] = it
	}
	list := make([]*item, 0, len(existing))
	for _, it := range existing {
		list = append(list, it)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if newestFirst {
			a, b = b, a
		}
		if !a.created.Equal(b.created) {
			return a.created.Before(b.created)
		}
		return a.URI < b.URI
	})
	return list, added
}

func writePosts(ctx context.Context, run *plugin.Run, did, handle string, posts []*item) error {
	byMonth := map[string][]*item{}
	for _, it := range posts {
		var rec map[string]any
		json.Unmarshal(it.Record, &rec)
		blobsOfJSON(rec, func(id, mimeType string) {
			if it.Media == nil {
				it.Media = map[string]string{}
			}
			it.Media[id] = postMediaPath(id, mimeType, it.created)
		})
		byMonth[monthOf(it.created)] = append(byMonth[monthOf(it.created)], it)
	}
	months := make([]string, 0, len(byMonth))
	for m := range byMonth {
		months = append(months, m)
	}
	sort.Strings(months)
	for _, m := range months {
		if err := ctx.Err(); err != nil {
			return err
		}
		jsonPath := fmt.Sprintf("publicaciones/%s/%s.json", m[:4], m)
		existing, err := readItems(ctx, run, jsonPath)
		if err != nil {
			return err
		}
		list, added := merge(existing, byMonth[m], false)
		modTime := list[len(list)-1].created
		if _, err := run.Out.Write(ctx, plugin.File{Path: jsonPath, ModTime: modTime, Data: plugin.JSON(list), Mutable: true}); err != nil {
			return err
		}
		md := monthMarkdown(did, handle, m, list)
		if _, err := run.Out.Write(ctx, plugin.File{Path: strings.TrimSuffix(jsonPath, ".json") + ".md", ModTime: modTime, Data: md, Mutable: true}); err != nil {
			return err
		}
		run.Item(added)
	}
	return nil
}

// blobsOfJSON finds every blob a record points to, wherever it is, in the
// JSON form (links as {"$link": …}). Old records name blobs {cid, mimeType}.
func blobsOfJSON(v any, fn func(id, mimeType string)) {
	switch t := v.(type) {
	case map[string]any:
		if t["$type"] == "blob" {
			if ref, ok := t["ref"].(map[string]any); ok {
				if link, ok := ref["$link"].(string); ok {
					mimeType, _ := t["mimeType"].(string)
					fn(link, mimeType)
					return
				}
			}
		}
		if id, ok := t["cid"].(string); ok {
			if mimeType, ok := t["mimeType"].(string); ok {
				fn(id, mimeType)
				return
			}
		}
		for _, item := range t {
			blobsOfJSON(item, fn)
		}
	case []any:
		for _, item := range t {
			blobsOfJSON(item, fn)
		}
	}
}

var monthNames = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

func monthTitle(month string) string {
	var n int
	fmt.Sscanf(month[5:], "%d", &n)
	if n < 1 || n > 12 {
		return "sin fecha"
	}
	return monthNames[n-1] + " de " + month[:4]
}

// webLink turns an at:// address into its page on bsky.app, which also opens
// with a DID instead of a handle (handles can change, DIDs do not).
func webLink(uri string) string {
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return uri
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return uri
	}
	switch parts[1] {
	case "app.bsky.feed.post":
		return "https://bsky.app/profile/" + parts[0] + "/post/" + parts[2]
	case "app.bsky.graph.list":
		return "https://bsky.app/profile/" + parts[0] + "/lists/" + parts[2]
	case "app.bsky.feed.generator":
		return "https://bsky.app/profile/" + parts[0] + "/feed/" + parts[2]
	}
	return uri
}

func altText(s string) string {
	return strings.NewReplacer("[", "(", "]", ")").Replace(strings.Join(strings.Fields(s), " "))
}

func writeEmbed(b *bytes.Buffer, e *embed, media map[string]string) {
	if e == nil {
		return
	}
	link := func(ref *blobRef) string {
		if local := media[ref.id()]; local != "" {
			return "../../" + local // from publicaciones/AAAA/ up to the folder
		}
		return ref.id()
	}
	for _, img := range e.Images {
		if img.Image == nil {
			continue
		}
		fmt.Fprintf(b, "![%s](%s)\n\n", altText(img.Alt), link(img.Image))
		if alt := altText(img.Alt); alt != "" {
			fmt.Fprintf(b, "_Texto alternativo: %s_\n\n", alt)
		}
	}
	if e.Video != nil {
		fmt.Fprintf(b, "[vídeo](%s)\n\n", link(e.Video))
		if alt := altText(e.Alt); alt != "" {
			fmt.Fprintf(b, "_Texto alternativo: %s_\n\n", alt)
		}
	}
	if e.External != nil && e.External.URI != "" {
		title := strings.Join(strings.Fields(e.External.Title), " ")
		if title == "" {
			title = e.External.URI
		}
		fmt.Fprintf(b, "Enlace: [%s](%s)\n\n", altText(title), e.External.URI)
	}
	if e.Record != nil {
		quoted := e.Record.URI
		if e.Record.Record != nil {
			quoted = e.Record.Record.URI
		}
		if quoted != "" {
			fmt.Fprintf(b, "_Cita: <%s>_\n\n", webLink(quoted))
		}
	}
	writeEmbed(b, e.Media, media)
}

func monthMarkdown(did, handle, month string, list []*item) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Publicaciones de @%s: %s\n\n", handle, monthTitle(month))
	for _, it := range list {
		var p postRecord
		json.Unmarshal(it.Record, &p)
		fmt.Fprintf(&b, "## %s\n\n", it.created.Format("2006-01-02 15:04"))
		if p.Reply != nil && p.Reply.Parent.URI != "" {
			fmt.Fprintf(&b, "_En respuesta a <%s>_\n\n", webLink(p.Reply.Parent.URI))
		}
		if text := strings.TrimSpace(p.Text); text != "" {
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				lines[i] = strings.TrimRight(line, " ")
			}
			b.WriteString(strings.Join(lines, "  \n")) // keep the line breaks
			b.WriteString("\n\n")
		}
		// Bluesky shortens links in the text («ejemplo.org/un-artic…») and
		// keeps the whole address in a facet: show the ones not in the text.
		for _, f := range p.Facets {
			for _, feature := range f.Features {
				if feature.Type == "app.bsky.richtext.facet#link" && feature.URI != "" && !strings.Contains(p.Text, feature.URI) {
					fmt.Fprintf(&b, "Enlace: <%s>\n\n", feature.URI)
				}
			}
		}
		writeEmbed(&b, p.Embed, it.Media)
		fmt.Fprintf(&b, "<%s>\n\n", webLink(it.URI))
	}
	return b.Bytes()
}

// writeSubjects saves likes or reposts: what was liked, newest first, merged
// with the copy so an undone like stays.
func writeSubjects(ctx context.Context, run *plugin.Run, name, title string, fresh []*item) error {
	existing, err := readItems(ctx, run, name+".json")
	if err != nil {
		return err
	}
	if len(existing) == 0 && len(fresh) == 0 {
		return nil
	}
	list, added := merge(existing, fresh, true)
	var md bytes.Buffer
	fmt.Fprintf(&md, "# %s\n\n", title)
	for _, it := range list {
		var rec subjectRecord
		json.Unmarshal(it.Record, &rec)
		var subject strongRef
		json.Unmarshal(rec.Subject, &subject)
		fmt.Fprintf(&md, "- %s <%s>\n", it.created.Format("2006-01-02 15:04"), webLink(subject.URI))
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: name + ".json", ModTime: list[0].created, Data: plugin.JSON(list), Mutable: true}); err != nil {
		return err
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: name + ".md", ModTime: list[0].created, Data: md.Bytes(), Mutable: true}); err != nil {
		return err
	}
	run.Item(added)
	return nil
}

// writeFollows saves who is followed now. It is a picture of the present, not
// a sum: the NAS keeps the previous versions, and the CARs the history.
func writeFollows(ctx context.Context, run *plugin.Run, follows []*item) error {
	list, _ := merge(map[string]*item{}, follows, true)
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write([]string{"did", "desde", "perfil"})
	for _, it := range list {
		var rec subjectRecord
		json.Unmarshal(it.Record, &rec)
		var did string
		json.Unmarshal(rec.Subject, &did)
		w.Write([]string{did, it.created.Format(time.RFC3339), "https://bsky.app/profile/" + did})
	}
	w.Flush()
	if _, err := run.Out.Write(ctx, plugin.File{Path: "seguidos.json", ModTime: run.Now(), Data: plugin.JSON(list), Mutable: true}); err != nil {
		return err
	}
	_, err := run.Out.Write(ctx, plugin.File{Path: "seguidos.csv", ModTime: run.Now(), Data: buf.Bytes(), Mutable: true})
	return err
}

func writeLists(ctx context.Context, run *plugin.Run, handle string, lists, items []*item) error {
	if len(lists) == 0 {
		return nil
	}
	type savedList struct {
		*item
		Items []*item `json:"items"`
	}
	members := map[string][]*item{}
	for _, it := range items {
		var rec subjectRecord
		json.Unmarshal(it.Record, &rec)
		members[rec.List] = append(members[rec.List], it)
	}
	out := []savedList{}
	var md bytes.Buffer
	fmt.Fprintf(&md, "# Listas de @%s\n\n", handle)
	sorted, _ := merge(map[string]*item{}, lists, false)
	for _, l := range sorted {
		var rec subjectRecord
		json.Unmarshal(l.Record, &rec)
		ms, _ := merge(map[string]*item{}, members[l.URI], false)
		out = append(out, savedList{item: l, Items: ms})
		fmt.Fprintf(&md, "## %s\n\n<%s>\n\n", strings.Join(strings.Fields(rec.Name), " "), webLink(l.URI))
		for _, m := range ms {
			var mr subjectRecord
			json.Unmarshal(m.Record, &mr)
			var did string
			json.Unmarshal(mr.Subject, &did)
			fmt.Fprintf(&md, "- <https://bsky.app/profile/%s>\n", did)
		}
		md.WriteString("\n")
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: "listas.json", ModTime: run.Now(), Data: plugin.JSON(out), Mutable: true}); err != nil {
		return err
	}
	_, err := run.Out.Write(ctx, plugin.File{Path: "listas.md", ModTime: run.Now(), Data: md.Bytes(), Mutable: true})
	return err
}

// ---- Blobs ----

// listBlobs adds to the pending list the blobs uploaded since the last
// listing. listBlobs with «since» gives only the new ones, so the cursor does
// not have to remember every CID ever downloaded.
func listBlobs(ctx context.Context, run *plugin.Run, c *client, cur *cursor, index map[string]pendingBlob) error {
	if cur.Pending == nil {
		cur.Pending = map[string]pendingBlob{}
	}
	params := url.Values{"did": {cur.DID}, "limit": {"500"}}
	if cur.BlobsRev != "" {
		params.Set("since", cur.BlobsRev)
	}
	for {
		var page struct {
			Cursor string   `json:"cursor"`
			CIDs   []string `json:"cids"`
		}
		if err := c.get(ctx, "com.atproto.sync.listBlobs", params, &page); err != nil {
			return err
		}
		for _, id := range page.CIDs {
			if _, err := parseCID(id); err != nil {
				continue
			}
			cur.Pending[id] = index[id]
		}
		if page.Cursor == "" || len(page.CIDs) == 0 {
			break
		}
		params.Set("cursor", page.Cursor)
	}
	cur.BlobsRev = cur.Rev
	return run.Save(*cur)
}

func downloadBlobs(ctx context.Context, run *plugin.Run, c *client, cur *cursor, cacheDir string) error {
	if len(cur.Pending) == 0 {
		return nil
	}
	ids := make([]string, 0, len(cur.Pending))
	for id := range cur.Pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	run.Logf("Descargando %d fotos y vídeos", len(ids))
	failed := 0
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		blob := cur.Pending[id]
		file, contentType, err := c.download(ctx, cacheDir, "com.atproto.sync.getBlob", url.Values{"did": {cur.DID}, "cid": {id}}, maxBlob)
		if err != nil {
			if strings.Contains(err.Error(), "límite de peticiones") {
				return err
			}
			// Kept as pending: tried again next time.
			run.Logf("No se pudo descargar %s: %s", id, err)
			failed++
			continue
		}
		rel := blob.Path
		if rel == "" {
			rel = "media/otros/" + plugin.SafeName(id+extension(contentType), 120)
		}
		modTime := run.Now()
		if blob.Time != 0 {
			modTime = time.Unix(blob.Time, 0)
		}
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		_, err = run.Out.Write(ctx, plugin.File{Path: rel, ModTime: modTime, Size: info.Size(),
			Open: func() (io.ReadCloser, error) { return os.Open(file) }})
		os.Remove(file)
		if err != nil {
			return err
		}
		delete(cur.Pending, id)
		if i%20 == 19 {
			if err := run.Save(*cur); err != nil {
				return err
			}
		}
	}
	if failed > 0 {
		run.Logf("%d ficheros quedan para la próxima vez", failed)
	}
	return run.Save(*cur)
}
