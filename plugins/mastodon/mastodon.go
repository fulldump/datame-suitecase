// Package mastodon keeps a copy of an account on any server with the Mastodon
// API (Mastodon, GoToSocial, Pleroma, Akkoma…): own posts (with boosts and
// replies) by month in JSON and Markdown with their original media, favourites,
// bookmarks, who follows and is followed, lists and the profile.
//
// Posts are fetched once: the first run walks the whole timeline backwards
// (max_id) and later runs ask only for what is newer than the newest seen
// (min_id). The months are merged with what is already on the NAS, so a post
// deleted online stays in the copy.
package mastodon

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
	"sort"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the Mastodon integration.
type Plugin struct{}

const pageSize = "40" // the most Mastodon gives per page of posts

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:  "mastodon",
		Title: "Mastodon (y el resto del fediverso)",
		Description: "Copia tu cuenta de Mastodon, GoToSocial, Pleroma o Akkoma: publicaciones por meses en JSON y Markdown con sus fotos y vídeos, " +
			"favoritos, guardados, seguidores, seguidos, listas y perfil.",
		Help: "En la web de tu servidor ve a Preferencias → Desarrollo → Nueva aplicación. Ponle un nombre (por ejemplo «Datame»), " +
			"desmarca todos los permisos y marca solo los de lectura: read:accounts, read:statuses, read:favourites, read:bookmarks, read:follows y read:lists. " +
			"Guarda, entra en la aplicación y copia «Tu token de acceso». Con esos permisos solo puede leer: no puede publicar ni borrar nada.",
		DefaultFolder:     "/Archivo/Mastodon",
		DefaultEveryHours: 6,
		Fields: []plugin.Field{
			{Name: "instance", Label: "Servidor", Type: "url", Required: true, Placeholder: "https://mastodon.social", Help: "La dirección de tu servidor, la parte de después de la segunda @ de tu cuenta."},
			{Name: "token", Label: "Token de acceso", Type: "password", Required: true, Secret: true, Help: "Preferencias → Desarrollo → Nueva aplicación, solo con permisos de lectura (read:…)."},
			{Name: "posts", Label: "Mis publicaciones (con impulsos y respuestas)", Type: "checkbox", Default: "true"},
			{Name: "media", Label: "Descargar las fotos y vídeos de mis publicaciones", Type: "checkbox", Default: "true"},
			{Name: "favourites", Label: "Favoritos", Type: "checkbox", Default: "true"},
			{Name: "bookmarks", Label: "Guardados", Type: "checkbox", Default: "true"},
			{Name: "follows", Label: "Seguidores y seguidos", Type: "checkbox", Default: "true"},
			{Name: "lists", Label: "Listas", Type: "checkbox", Default: "true"},
		},
	}
}

// on reads a checkbox that is on unless it says otherwise, so a config saved
// before a new option existed keeps copying everything.
func on(config plugin.Config, name string) bool { return config.Get(name) != "false" }

// Validate implements plugin.Plugin: the token must work on that server.
func (p Plugin) Validate(ctx context.Context, env plugin.Env) error {
	if err := plugin.CheckRequired(p.Info(), env.Config); err != nil {
		return err
	}
	c, err := newClient(env.HTTP, env.Config.Get("instance"), env.Config.Get("token"))
	if err != nil {
		return err
	}
	var me account
	if _, err := c.get(ctx, "/api/v1/accounts/verify_credentials", &me); err != nil {
		if errors.Is(err, errNotFound) {
			return fmt.Errorf("%s no parece un servidor con la API de Mastodon", c.instance.Host)
		}
		return err
	}
	if me.ID == "" {
		return fmt.Errorf("%s no ha devuelto la cuenta", c.instance.Host)
	}
	return nil
}

// ---- What the API returns (only what is read; the JSON is kept whole) ----

type account struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Acct        string `json:"acct"`
	DisplayName string `json:"display_name"`
	URL         string `json:"url"`
	Avatar      string `json:"avatar"`
	Header      string `json:"header"`
}

type attachment struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	RemoteURL   string `json:"remote_url"`
	Description string `json:"description"`
}

type status struct {
	ID                 string       `json:"id"`
	URL                string       `json:"url"`
	URI                string       `json:"uri"`
	CreatedAt          time.Time    `json:"created_at"`
	EditedAt           string       `json:"edited_at"`
	Visibility         string       `json:"visibility"`
	SpoilerText        string       `json:"spoiler_text"`
	Content            string       `json:"content"`
	InReplyToID        string       `json:"in_reply_to_id"`
	InReplyToAccountID string       `json:"in_reply_to_account_id"`
	Account            account      `json:"account"`
	Reblog             *status      `json:"reblog"`
	MediaAttachments   []attachment `json:"media_attachments"`
	Mentions           []struct {
		ID   string `json:"id"`
		Acct string `json:"acct"`
		URL  string `json:"url"`
	} `json:"mentions"`
	Poll *struct {
		Options []struct {
			Title      string `json:"title"`
			VotesCount *int   `json:"votes_count"`
		} `json:"options"`
	} `json:"poll"`
	FavouritesCount int `json:"favourites_count"`
	ReblogsCount    int `json:"reblogs_count"`
}

// attachments returns the media of a post, or of the boosted post.
func (s *status) attachments() []attachment {
	if s.Reblog != nil {
		return s.Reblog.MediaAttachments
	}
	return s.MediaAttachments
}

// entry is one post as saved in the month file: the status exactly as the API
// gave it, its edit history when it was edited, and where its media is.
type entry struct {
	Status  json.RawMessage   `json:"status"`
	Edits   []json.RawMessage `json:"edits,omitempty"`    // /api/v1/statuses/:id/history, oldest first
	EditsOf string            `json:"edits_of,omitempty"` // the edited_at those edits are for
	Media   map[string]string `json:"local_media,omitempty"`

	parsed status
}

// ---- Cursor ----

type statusCursor struct {
	Newest string `json:"newest,omitempty"` // newest post id seen: the next run asks for min_id=this
	Oldest string `json:"oldest,omitempty"` // oldest seen while walking back the first time
	Done   bool   `json:"done,omitempty"`   // the walk back reached the first post
}

type pagedCursor struct {
	Done bool   `json:"done,omitempty"` // the whole list was read once
	Next string `json:"next,omitempty"` // where the first read stopped, to go on
}

type cursor struct {
	AccountID  string       `json:"account_id,omitempty"`
	Statuses   statusCursor `json:"statuses"`
	Favourites pagedCursor  `json:"favourites"`
	Bookmarks  pagedCursor  `json:"bookmarks"`
	Avatar     string       `json:"avatar,omitempty"` // URL of the avatar already saved
	Header     string       `json:"header,omitempty"`
}

// ---- Sync ----

// Sync implements plugin.Plugin.
func (p Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	c, err := newClient(run.HTTP, run.Config.Get("instance"), run.Config.Get("token"))
	if err != nil {
		return err
	}
	var cur cursor
	if err := run.Load(&cur); err != nil {
		return err
	}
	var meRaw json.RawMessage
	if _, err := c.get(ctx, "/api/v1/accounts/verify_credentials", &meRaw); err != nil {
		return err
	}
	var me account
	if err := json.Unmarshal(meRaw, &me); err != nil || me.ID == "" {
		return fmt.Errorf("%s no ha devuelto la cuenta", c.instance.Host)
	}
	if cur.AccountID != "" && cur.AccountID != me.ID {
		// Another account in the same folder: start its timeline from scratch
		// (what is on the NAS is merged, not lost).
		run.Logf("El token es de otra cuenta (@%s): se empieza de nuevo", me.Acct)
		cur = cursor{}
	}
	cur.AccountID = me.ID
	handle := me.Acct
	if !strings.Contains(handle, "@") {
		handle += "@" + c.instance.Host
	}

	if err := writeProfile(ctx, run, c, &cur, meRaw, me); err != nil {
		return err
	}
	if err := run.Save(cur); err != nil {
		return err
	}
	if on(run.Config, "posts") {
		if err := syncStatuses(ctx, run, c, &cur, me, handle); err != nil {
			return err
		}
	}
	if on(run.Config, "favourites") {
		if err := syncSaved(ctx, run, c, &cur, &cur.Favourites, "/api/v1/favourites?limit="+pageSize, "favoritos", "Favoritos de @"+handle); err != nil {
			return err
		}
	}
	if on(run.Config, "bookmarks") {
		if err := syncSaved(ctx, run, c, &cur, &cur.Bookmarks, "/api/v1/bookmarks?limit="+pageSize, "guardados", "Guardados de @"+handle); err != nil {
			return err
		}
	}
	if on(run.Config, "follows") {
		for _, f := range []struct{ endpoint, name string }{{"followers", "seguidores"}, {"following", "seguidos"}} {
			if err := syncAccounts(ctx, run, c, "/api/v1/accounts/"+url.PathEscape(me.ID)+"/"+f.endpoint+"?limit=80", f.name); err != nil {
				return err
			}
		}
	}
	if on(run.Config, "lists") {
		if err := syncLists(ctx, run, c, handle); err != nil {
			return err
		}
	}
	return nil
}

// idLess compares ids. Mastodon ids are numbers in a string, GoToSocial ids
// are ULIDs and Pleroma ids are flake ids: in all of them a longer id is newer,
// and ids of the same length sort as text.
func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

type collected struct {
	raw     json.RawMessage
	parsed  status
	refresh bool // came only from re-reading the latest page, to catch edits
}

// syncStatuses brings the posts and writes them. Whatever happens, what was
// fetched is written before the cursor moves, so nothing is skipped.
func syncStatuses(ctx context.Context, run *plugin.Run, c *client, cur *cursor, me account, handle string) error {
	base := "/api/v1/accounts/" + url.PathEscape(me.ID) + "/statuses?limit=" + pageSize
	next := cur.Statuses
	coll := map[string]*collected{}
	add := func(page []json.RawMessage, refresh bool) (ids []string) {
		for _, raw := range page {
			var s status
			if json.Unmarshal(raw, &s) != nil || s.ID == "" {
				continue
			}
			if old, ok := coll[s.ID]; ok && !old.refresh {
				continue
			}
			coll[s.ID] = &collected{raw: raw, parsed: s, refresh: refresh}
			ids = append(ids, s.ID)
		}
		return ids
	}

	fetchErr := func() error {
		// First, the walk back to the very first post (only until done).
		for !next.Done {
			uri := base
			if next.Oldest != "" {
				uri += "&max_id=" + url.QueryEscape(next.Oldest)
			}
			var page []json.RawMessage
			if _, err := c.get(ctx, uri, &page); err != nil {
				return err
			}
			moved := false
			for _, id := range add(page, false) {
				if next.Newest == "" || idLess(next.Newest, id) {
					next.Newest = id
				}
				if next.Oldest == "" || idLess(id, next.Oldest) {
					next.Oldest = id
					moved = true
				}
			}
			if len(page) == 0 || !moved {
				next.Done = true
			}
			run.Logf("Leídas %d publicaciones", len(coll))
		}
		// Then what is newer than the newest seen. With min_id Mastodon gives
		// the posts right after it, so this walks forward page by page.
		for next.Newest != "" {
			var page []json.RawMessage
			if _, err := c.get(ctx, base+"&min_id="+url.QueryEscape(next.Newest), &page); err != nil {
				return err
			}
			moved := false
			for _, id := range add(page, false) {
				if idLess(next.Newest, id) {
					next.Newest = id
					moved = true
				}
			}
			if !moved {
				break
			}
		}
		// Last, the latest page again: an edit does not change the id, so it is
		// only seen by reading the post again. Done only when the timeline was
		// complete before this run, since a first run has just read it.
		if cur.Statuses.Done {
			var page []json.RawMessage
			if _, err := c.get(ctx, base, &page); err != nil {
				return err
			}
			add(page, true)
		}
		return nil
	}()

	if len(coll) > 0 {
		if err := writeStatuses(ctx, run, c, coll, handle); err != nil {
			return err
		}
	}
	cur.Statuses = next
	if err := run.Save(*cur); err != nil {
		return err
	}
	return fetchErr
}

func monthOf(t time.Time) string {
	if t.IsZero() {
		return "0000-00"
	}
	return t.UTC().Format("2006-01")
}

func writeStatuses(ctx context.Context, run *plugin.Run, c *client, coll map[string]*collected, handle string) error {
	byMonth := map[string][]*collected{}
	for _, item := range coll {
		m := monthOf(item.parsed.CreatedAt)
		byMonth[m] = append(byMonth[m], item)
	}
	months := make([]string, 0, len(byMonth))
	for m := range byMonth {
		months = append(months, m)
	}
	sort.Strings(months)
	tmp, err := tempDir(run.CacheDir)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, m := range months {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeMonth(ctx, run, c, tmp, m, byMonth[m], handle); err != nil {
			return err
		}
	}
	return nil
}

func tempDir(cacheDir string) (string, error) {
	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			return "", err
		}
	}
	return os.MkdirTemp(cacheDir, "mastodon-")
}

func readMonth(ctx context.Context, run *plugin.Run, jsonPath string) (map[string]*entry, error) {
	merged := map[string]*entry{}
	data, err := run.Out.Read(ctx, jsonPath)
	if errors.Is(err, plugin.ErrNotExist) {
		return merged, nil
	}
	if err != nil {
		return nil, err
	}
	var old []*entry
	if err := json.Unmarshal(data, &old); err != nil {
		return nil, fmt.Errorf("%s en el NAS no se puede leer: %w", jsonPath, err)
	}
	for _, e := range old {
		if json.Unmarshal(e.Status, &e.parsed) == nil && e.parsed.ID != "" {
			merged[e.parsed.ID] = e
		}
	}
	return merged, nil
}

func writeMonth(ctx context.Context, run *plugin.Run, c *client, tmp, month string, items []*collected, handle string) error {
	jsonPath := fmt.Sprintf("publicaciones/%s/%s.json", month[:4], month)
	merged, err := readMonth(ctx, run, jsonPath)
	if err != nil {
		return err
	}
	added := 0
	media := on(run.Config, "media")
	for _, item := range items {
		old, exists := merged[item.parsed.ID]
		if exists && item.refresh && old.parsed.EditedAt == item.parsed.EditedAt {
			// Re-read only to look for edits, and it was not edited: keep the
			// copy as it is, so counters going up do not make new versions.
			continue
		}
		e := &entry{Status: item.raw, parsed: item.parsed}
		if exists {
			e.Media = old.Media
			if old.EditsOf == item.parsed.EditedAt {
				e.Edits, e.EditsOf = old.Edits, old.EditsOf
			}
		} else {
			added++
		}
		if item.parsed.EditedAt != "" && e.EditsOf != item.parsed.EditedAt {
			var history []json.RawMessage
			if _, err := c.get(ctx, "/api/v1/statuses/"+url.PathEscape(item.parsed.ID)+"/history", &history); err == nil {
				e.Edits, e.EditsOf = history, item.parsed.EditedAt
			} else {
				// The post itself is saved anyway; the history is a bonus.
				run.Logf("No se pudo leer el historial de ediciones de %s: %s", item.parsed.ID, err)
			}
		}
		if media {
			if err := saveMedia(ctx, run, c, tmp, e); err != nil {
				return err
			}
		}
		merged[item.parsed.ID] = e
	}

	list := make([]*entry, 0, len(merged))
	for _, e := range merged {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i].parsed, list[j].parsed
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return idLess(a.ID, b.ID)
	})
	modTime := list[len(list)-1].parsed.CreatedAt
	if _, err := run.Out.Write(ctx, plugin.File{Path: jsonPath, ModTime: modTime, Data: plugin.JSON(list), Mutable: true}); err != nil {
		return err
	}
	md := monthMarkdown(handle, month, list)
	if _, err := run.Out.Write(ctx, plugin.File{Path: strings.TrimSuffix(jsonPath, ".json") + ".md", ModTime: modTime, Data: md, Mutable: true}); err != nil {
		return err
	}
	run.Item(added)
	return nil
}

// saveMedia downloads the attachments of a post that are not on the NAS yet.
// A failed download is logged and tried again the next time the post is read.
func saveMedia(ctx context.Context, run *plugin.Run, c *client, tmp string, e *entry) error {
	for _, a := range e.parsed.attachments() {
		if a.ID == "" || e.Media[a.ID] != "" {
			continue
		}
		src := a.URL
		if src == "" {
			src = a.RemoteURL
		}
		if src == "" {
			continue
		}
		file, lastModified, err := c.download(ctx, tmp, src)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			run.Logf("No se pudo descargar %s: %s", src, err)
			continue
		}
		u, _ := url.Parse(src)
		created := e.parsed.CreatedAt.UTC()
		rel := fmt.Sprintf("media/%s/%s/%s", monthOf(created)[:4], monthOf(created), plugin.SafeName(a.ID+"-"+path.Base(u.Path), 120))
		modTime := created
		if modTime.IsZero() {
			modTime = lastModified
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
		if e.Media == nil {
			e.Media = map[string]string{}
		}
		e.Media[a.ID] = rel
	}
	return nil
}

// ---- Markdown ----

var monthNames = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

var visibilities = map[string]string{
	"public":   "pública",
	"unlisted": "no listada",
	"private":  "solo seguidores",
	"direct":   "mención privada",
}

func monthTitle(month string) string {
	var n int
	fmt.Sscanf(month[5:], "%d", &n)
	if n < 1 || n > 12 {
		return "sin fecha"
	}
	return monthNames[n-1] + " de " + month[:4]
}

func monthMarkdown(handle, month string, list []*entry) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Publicaciones de @%s: %s\n\n", handle, monthTitle(month))
	for _, e := range list {
		s := e.parsed
		fmt.Fprintf(&b, "## %s", s.CreatedAt.UTC().Format("2006-01-02 15:04"))
		if v := visibilities[s.Visibility]; v != "" {
			fmt.Fprintf(&b, " · %s", v)
		}
		b.WriteString("\n\n")
		shown := &s
		if s.Reblog != nil {
			fmt.Fprintf(&b, "_Impulso de una publicación de @%s (%s)_\n\n", s.Reblog.Account.Acct, s.Reblog.CreatedAt.UTC().Format("2006-01-02 15:04"))
			shown = s.Reblog
		}
		if shown.InReplyToID != "" {
			to := ""
			for _, m := range shown.Mentions {
				if m.ID == shown.InReplyToAccountID {
					to = "@" + m.Acct
				}
			}
			if to == "" && shown.InReplyToAccountID == shown.Account.ID {
				to = "sí mismo (hilo)"
			}
			if to == "" {
				to = "otra publicación"
			}
			fmt.Fprintf(&b, "_En respuesta a %s_\n\n", to)
		}
		if shown.SpoilerText != "" {
			fmt.Fprintf(&b, "**Aviso de contenido:** %s\n\n", oneLine(shown.SpoilerText))
		}
		if text := htmlToMarkdown(shown.Content); text != "" {
			b.WriteString(text)
			b.WriteString("\n\n")
		}
		if shown.Poll != nil && len(shown.Poll.Options) > 0 {
			b.WriteString("Encuesta:\n\n")
			for _, o := range shown.Poll.Options {
				if o.VotesCount != nil {
					fmt.Fprintf(&b, "- %s (%d votos)\n", oneLine(o.Title), *o.VotesCount)
				} else {
					fmt.Fprintf(&b, "- %s\n", oneLine(o.Title))
				}
			}
			b.WriteString("\n")
		}
		for _, a := range shown.MediaAttachments {
			link := a.URL
			if local := e.Media[a.ID]; local != "" {
				link = "../../" + local // from publicaciones/AAAA/ up to the folder
			} else if link == "" {
				link = a.RemoteURL
			}
			alt := oneLine(a.Description)
			switch a.Type {
			case "image", "gifv":
				fmt.Fprintf(&b, "![%s](%s)\n\n", strings.NewReplacer("[", "(", "]", ")").Replace(alt), link)
			default:
				label := map[string]string{"video": "vídeo", "audio": "audio"}[a.Type]
				if label == "" {
					label = "adjunto"
				}
				fmt.Fprintf(&b, "[%s](%s)\n\n", label, link)
			}
			if alt != "" {
				fmt.Fprintf(&b, "_Texto alternativo: %s_\n\n", alt)
			}
		}
		link := shown.URL
		if link == "" {
			link = shown.URI
		}
		if link != "" {
			fmt.Fprintf(&b, "<%s>", link)
		}
		if s.Reblog == nil && (s.FavouritesCount > 0 || s.ReblogsCount > 0) {
			fmt.Fprintf(&b, " · %d favoritos · %d impulsos", s.FavouritesCount, s.ReblogsCount)
		}
		if shown.EditedAt != "" {
			fmt.Fprintf(&b, " · editada")
			if t, err := time.Parse(time.RFC3339Nano, shown.EditedAt); err == nil {
				fmt.Fprintf(&b, " el %s", t.UTC().Format("2006-01-02 15:04"))
			}
			if len(e.Edits) > 1 {
				fmt.Fprintf(&b, " (%d versiones en el JSON)", len(e.Edits))
			}
		}
		b.WriteString("\n\n")
	}
	return b.Bytes()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ---- Favourites and bookmarks ----

// syncSaved keeps a list of posts of other people (favourites, bookmarks). The
// API orders them by when they were saved, with opaque page ids, so: the first
// time the whole list is read (going on where it stopped if cut), and later
// only until a post already in the copy shows up.
func syncSaved(ctx context.Context, run *plugin.Run, c *client, cur *cursor, state *pagedCursor, first, name, title string) error {
	jsonPath := name + ".json"
	var old []json.RawMessage
	data, err := run.Out.Read(ctx, jsonPath)
	if err != nil && !errors.Is(err, plugin.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &old); err != nil {
			return fmt.Errorf("%s en el NAS no se puede leer: %w", jsonPath, err)
		}
	}
	idOf := func(raw json.RawMessage) string {
		var head struct {
			ID string `json:"id"`
		}
		json.Unmarshal(raw, &head)
		return head.ID
	}
	known := map[string]bool{}
	for _, raw := range old {
		known[idOf(raw)] = true
	}

	next := *state
	resuming := !state.Done && state.Next != ""
	uri := first
	if resuming {
		uri = state.Next
	}
	fresh := []json.RawMessage{}
	seen := map[string]bool{}
	var fetchErr error
	for uri != "" {
		var page []json.RawMessage
		nextURI, err := c.get(ctx, uri, &page)
		if err != nil {
			fetchErr = err
			break
		}
		reachedKnown := false
		for _, raw := range page {
			id := idOf(raw)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			fresh = append(fresh, raw)
			if known[id] {
				reachedKnown = true
			}
		}
		if len(page) == 0 || nextURI == "" {
			next.Done, next.Next = true, ""
			break
		}
		if !next.Done {
			next.Next = nextURI
		}
		if reachedKnown && state.Done {
			break
		}
		uri = nextURI
	}

	// The newest first. Pages read while resuming the first walk are older
	// than what the copy has; otherwise they are newer.
	var list []json.RawMessage
	added := 0
	if resuming {
		list = append(list, old...)
		for _, raw := range fresh {
			if !known[idOf(raw)] {
				list = append(list, raw)
				added++
			}
		}
	} else {
		for _, raw := range fresh {
			if !known[idOf(raw)] {
				added++
			}
		}
		list = append(list, fresh...)
		for _, raw := range old {
			if !seen[idOf(raw)] {
				list = append(list, raw)
			}
		}
	}
	if len(list) > 0 {
		if _, err := run.Out.Write(ctx, plugin.File{Path: jsonPath, ModTime: run.Now(), Data: plugin.JSON(list), Mutable: true}); err != nil {
			return err
		}
		if _, err := run.Out.Write(ctx, plugin.File{Path: name + ".md", ModTime: run.Now(), Data: savedMarkdown(title, list), Mutable: true}); err != nil {
			return err
		}
	}
	run.Item(added)
	*state = next
	if err := run.Save(*cur); err != nil {
		return err
	}
	return fetchErr
}

func savedMarkdown(title string, list []json.RawMessage) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n", title)
	for _, raw := range list {
		var s status
		json.Unmarshal(raw, &s)
		text := oneLine(htmlToMarkdown(s.Content))
		if s.SpoilerText != "" {
			text = "[" + oneLine(s.SpoilerText) + "] " + text
		}
		if r := []rune(text); len(r) > 280 {
			text = string(r[:280]) + "…"
		}
		link := s.URL
		if link == "" {
			link = s.URI
		}
		fmt.Fprintf(&b, "- %s · @%s: %s <%s>\n", s.CreatedAt.UTC().Format("2006-01-02"), s.Account.Acct, text, link)
	}
	return b.Bytes()
}

// ---- Followers, following and lists ----

// allPages reads every page of a list of accounts.
func allPages(ctx context.Context, c *client, uri string) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	for uri != "" {
		var page []json.RawMessage
		next, err := c.get(ctx, uri, &page)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		out = append(out, page...)
		uri = next
	}
	return out, nil
}

// syncAccounts saves who follows or is followed as it is now. Unlike posts,
// this is a picture of the present: someone who unfollowed is no longer in
// it, and the NAS keeps the previous versions of the file.
func syncAccounts(ctx context.Context, run *plugin.Run, c *client, uri, name string) error {
	list, err := allPages(ctx, c, uri)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	// «Account address» first, as Mastodon names it in its own exports, so
	// the file can be imported into another server.
	w.Write([]string{"Account address", "Nombre", "URL"})
	for _, raw := range list {
		var a account
		json.Unmarshal(raw, &a)
		acct := a.Acct
		if acct != "" && !strings.Contains(acct, "@") {
			acct += "@" + c.instance.Host
		}
		w.Write([]string{acct, oneLine(a.DisplayName), a.URL})
	}
	w.Flush()
	if _, err := run.Out.Write(ctx, plugin.File{Path: name + ".json", ModTime: run.Now(), Data: plugin.JSON(list), Mutable: true}); err != nil {
		return err
	}
	_, err = run.Out.Write(ctx, plugin.File{Path: name + ".csv", ModTime: run.Now(), Data: buf.Bytes(), Mutable: true})
	return err
}

func syncLists(ctx context.Context, run *plugin.Run, c *client, handle string) error {
	var lists []json.RawMessage
	if _, err := c.get(ctx, "/api/v1/lists", &lists); err != nil {
		return err
	}
	type savedList struct {
		List     json.RawMessage   `json:"list"`
		Accounts []json.RawMessage `json:"accounts"`
	}
	out := []savedList{}
	var md bytes.Buffer
	fmt.Fprintf(&md, "# Listas de @%s\n\n", handle)
	for _, raw := range lists {
		var l struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if json.Unmarshal(raw, &l) != nil || l.ID == "" {
			continue
		}
		accounts, err := allPages(ctx, c, "/api/v1/lists/"+url.PathEscape(l.ID)+"/accounts?limit=80")
		if err != nil {
			return err
		}
		out = append(out, savedList{List: raw, Accounts: accounts})
		fmt.Fprintf(&md, "## %s\n\n", oneLine(l.Title))
		for _, a := range accounts {
			var acc account
			json.Unmarshal(a, &acc)
			fmt.Fprintf(&md, "- @%s %s <%s>\n", acc.Acct, oneLine(acc.DisplayName), acc.URL)
		}
		md.WriteString("\n")
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: "listas.json", ModTime: run.Now(), Data: plugin.JSON(out), Mutable: true}); err != nil {
		return err
	}
	_, err := run.Out.Write(ctx, plugin.File{Path: "listas.md", ModTime: run.Now(), Data: md.Bytes(), Mutable: true})
	return err
}

// ---- Profile ----

func writeProfile(ctx context.Context, run *plugin.Run, c *client, cur *cursor, raw json.RawMessage, me account) error {
	if _, err := run.Out.Write(ctx, plugin.File{Path: "perfil.json", ModTime: run.Now(), Data: plugin.JSON(raw), Mutable: true}); err != nil {
		return err
	}
	tmp, err := tempDir(run.CacheDir)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, img := range []struct {
		name string
		src  string
		done *string
	}{{"avatar", me.Avatar, &cur.Avatar}, {"cabecera", me.Header, &cur.Header}} {
		if img.src == "" || img.src == *img.done || strings.HasSuffix(img.src, "/missing.png") {
			continue
		}
		file, lastModified, err := c.download(ctx, tmp, img.src)
		if err != nil {
			run.Logf("No se pudo descargar la imagen del perfil: %s", err)
			continue
		}
		// Named by its address: a new picture is a new file, the old one stays.
		u, _ := url.Parse(img.src)
		ext := strings.ToLower(path.Ext(u.Path))
		if len(ext) > 6 {
			ext = ""
		}
		rel := "perfil/" + img.name + "-" + plugin.ShortHash(img.src) + ext
		if lastModified.IsZero() {
			lastModified = run.Now()
		}
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		_, err = run.Out.Write(ctx, plugin.File{Path: rel, ModTime: lastModified, Size: info.Size(),
			Open: func() (io.ReadCloser, error) { return os.Open(file) }})
		os.Remove(file)
		if err != nil {
			return err
		}
		*img.done = img.src
	}
	return nil
}
