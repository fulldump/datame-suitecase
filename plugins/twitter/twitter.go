// Package twitter imports the archive that X (Twitter) gives to each account
// («Descargar un archivo de tus datos»). The API is paid, the archive is free
// and has everything: tweets, media, likes.
//
// Tweets go to one JSON and one Markdown file per month, merged with what is
// already on the NAS, so a newer archive adds and never removes (a tweet
// deleted in X stays in the copy). The ZIP itself is kept too, as it came.
package twitter

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the Twitter/X archive importer.
type Plugin struct{}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:        "twitter",
		Title:       "X / Twitter (archivo de la cuenta)",
		Description: "Importa el archivo oficial de tu cuenta: tuits por meses en JSON y Markdown, con sus fotos y vídeos, y tus «me gusta».",
		Help: "En X, ve a Más → Configuración y privacidad → Tu cuenta → Descargar un archivo de tus datos. " +
			"Te pedirá la contraseña y un código; el archivo tarda uno o varios días y llega un aviso para descargarlo. " +
			"Deja el ZIP (twitter-AAAA-MM-DD-….zip) en la carpeta que indiques aquí: cada vez que pongas uno nuevo se importará lo que falte. " +
			"La API de X es de pago, por eso se usa el archivo.",
		DefaultFolder:     "/Archivo/Twitter",
		DefaultEveryHours: 24,
		Fields: []plugin.Field{
			{Name: "path", Label: "ZIP del archivo, o carpeta donde lo dejas", Type: "path", Required: true, Placeholder: "/home/ana/Descargas/twitter", Help: "Una carpeta con uno o varios ZIP, un ZIP concreto, o el archivo ya descomprimido."},
			{Name: "keep_zip", Label: "Guardar también el ZIP original en el NAS", Type: "checkbox", Default: "true"},
		},
	}
}

// source is one archive: a ZIP or an unpacked folder.
type source struct {
	path string
	key  string // changes when the file changes
	zip  bool
}

func sources(root string) ([]source, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("no encuentro %s", root)
	}
	key := func(p string, i os.FileInfo) string {
		return fmt.Sprintf("%s|%d|%d", filepath.Base(p), i.Size(), i.ModTime().Unix())
	}
	if !info.IsDir() {
		return []source{{path: root, key: key(root, info), zip: true}}, nil
	}
	for _, name := range []string{"data/tweets.js", "data/tweet.js"} {
		if inner, err := os.Stat(filepath.Join(root, name)); err == nil {
			return []source{{path: root, key: key(root, inner)}}, nil
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := []source{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".zip") {
			continue
		}
		i, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, source{path: filepath.Join(root, entry.Name()), key: key(entry.Name(), i), zip: true})
	}
	// By name, which X makes «twitter-AAAA-MM-DD-…»: the oldest first, so a
	// newer archive merges over an older one.
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

// Validate implements plugin.Plugin.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error {
	list, err := sources(env.Config.Get("path"))
	if err != nil {
		return err
	}
	for _, s := range list {
		if !s.zip {
			continue
		}
		r, err := zip.OpenReader(s.path)
		if err != nil {
			return fmt.Errorf("%s no es un ZIP válido", filepath.Base(s.path))
		}
		_, err = tweetFiles(r)
		r.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(s.path), err)
		}
	}
	return nil
}

type cursor struct {
	Imported map[string]int64 `json:"imported"` // source key → when
}

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	c := cursor{Imported: map[string]int64{}}
	if err := run.Load(&c); err != nil {
		return err
	}
	if c.Imported == nil {
		c.Imported = map[string]int64{}
	}
	list, err := sources(run.Config.Get("path"))
	if err != nil {
		return err
	}
	pending := 0
	for _, s := range list {
		if c.Imported[s.key] != 0 {
			continue
		}
		pending++
		run.Logf("Importando %s", filepath.Base(s.path))
		if err := importSource(ctx, run, s); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(s.path), err)
		}
		c.Imported[s.key] = run.Now().Unix()
		if err := run.Save(c); err != nil {
			return err
		}
	}
	if pending == 0 {
		run.Logf("No hay archivos nuevos que importar")
	}
	return nil
}

func importSource(ctx context.Context, run *plugin.Run, s source) error {
	var fsys fs.FS
	if s.zip {
		r, err := zip.OpenReader(s.path)
		if err != nil {
			return err
		}
		defer r.Close()
		fsys = r
	} else {
		fsys = os.DirFS(s.path)
	}
	a, err := readArchive(fsys)
	if err != nil {
		return err
	}
	run.Logf("El archivo de @%s tiene %d tuits y %d «me gusta»", a.username, len(a.tweets), len(a.likes))
	if err := writeTweets(ctx, run, fsys, a); err != nil {
		return err
	}
	if err := writeLikes(ctx, run, a); err != nil {
		return err
	}
	if s.zip && run.Config.Get("keep_zip") != "false" {
		info, err := os.Stat(s.path)
		if err != nil {
			return err
		}
		_, err = run.Out.Write(ctx, plugin.File{Path: "originales/" + plugin.SafeName(filepath.Base(s.path), 120), ModTime: info.ModTime(), Size: info.Size(),
			Open: func() (io.ReadCloser, error) { return os.Open(s.path) }})
		return err
	}
	return nil
}

// ---- Reading the archive ----

type tweet struct {
	raw     json.RawMessage
	id      string
	created time.Time
}

type archive struct {
	username string
	tweets   []tweet
	likes    []json.RawMessage
}

var partFile = regexp.MustCompile(`^data/(tweets|tweet)(-part\d+)?\.js$`)

func tweetFiles(fsys fs.FS) ([]string, error) {
	matches := []string{}
	err := fs.WalkDir(fsys, "data", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != "data" {
			return fs.SkipDir
		}
		if partFile.MatchString(p) {
			matches = append(matches, p)
		}
		return nil
	})
	if err != nil || len(matches) == 0 {
		return nil, errors.New("no parece un archivo de X: falta data/tweets.js")
	}
	sort.Strings(matches)
	return matches, nil
}

// readJS reads one of the archive's .js files: «window.YTD.tweets.part0 = [...]».
func readJS(fsys fs.FS, name string) ([]json.RawMessage, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	if i := bytes.IndexByte(data, '='); i >= 0 && bytes.IndexByte(data, '[') > i {
		data = data[i+1:]
	}
	var items []json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(data), &items); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return items, nil
}

// unwrap takes {"tweet": {...}} to {...}; older archives have no wrapper.
func unwrap(item json.RawMessage, key string) json.RawMessage {
	var wrapper map[string]json.RawMessage
	if json.Unmarshal(item, &wrapper) == nil {
		if inner, ok := wrapper[key]; ok {
			return inner
		}
	}
	return item
}

const createdLayout = "Mon Jan 02 15:04:05 -0700 2006"

func readArchive(fsys fs.FS) (*archive, error) {
	a := &archive{username: "cuenta"}
	if items, err := readJS(fsys, "data/account.js"); err == nil && len(items) > 0 {
		var account struct {
			Username string `json:"username"`
		}
		if json.Unmarshal(unwrap(items[0], "account"), &account) == nil && account.Username != "" {
			a.username = account.Username
		}
	}
	files, err := tweetFiles(fsys)
	if err != nil {
		return nil, err
	}
	for _, name := range files {
		items, err := readJS(fsys, name)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			raw := unwrap(item, "tweet")
			var head struct {
				ID        string `json:"id_str"`
				CreatedAt string `json:"created_at"`
			}
			if err := json.Unmarshal(raw, &head); err != nil || head.ID == "" {
				continue
			}
			created, err := time.Parse(createdLayout, head.CreatedAt)
			if err != nil {
				continue
			}
			a.tweets = append(a.tweets, tweet{raw: raw, id: head.ID, created: created.UTC()})
		}
	}
	if items, err := readJS(fsys, "data/like.js"); err == nil {
		for _, item := range items {
			a.likes = append(a.likes, unwrap(item, "like"))
		}
	}
	return a, nil
}

// ---- Writing ----

func monthKey(t time.Time) string { return t.Format("2006-01") }

func writeTweets(ctx context.Context, run *plugin.Run, fsys fs.FS, a *archive) error {
	byMonth := map[string][]tweet{}
	dates := map[string]time.Time{}
	for _, t := range a.tweets {
		byMonth[monthKey(t.created)] = append(byMonth[monthKey(t.created)], t)
		dates[t.id] = t.created
	}

	// Media first: the Markdown links to it. Files are «<tweet id>-<name>».
	media := map[string][]string{} // tweet id → paths relative to the folder
	entries, _ := fs.ReadDir(fsys, "data/tweets_media")
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			continue
		}
		id, _, _ := strings.Cut(entry.Name(), "-")
		dir := "media/otros"
		if when, ok := dates[id]; ok {
			dir = fmt.Sprintf("media/%04d/%s", when.Year(), monthKey(when))
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel := dir + "/" + plugin.SafeName(entry.Name(), 120)
		name := "data/tweets_media/" + entry.Name()
		modTime := dates[id]
		if modTime.IsZero() {
			modTime = info.ModTime()
		}
		if _, err := run.Out.Write(ctx, plugin.File{Path: rel, ModTime: modTime, Size: info.Size(),
			Open: func() (io.ReadCloser, error) { return fsys.Open(name) }}); err != nil {
			return err
		}
		media[id] = append(media[id], rel)
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
		if err := writeMonth(ctx, run, a.username, m, byMonth[m], media); err != nil {
			return err
		}
	}
	return nil
}

// writeMonth merges the tweets of a month with the ones on the NAS and writes
// the JSON and the Markdown.
func writeMonth(ctx context.Context, run *plugin.Run, username, month string, tweets []tweet, media map[string][]string) error {
	jsonPath := fmt.Sprintf("tweets/%s/%s.json", month[:4], month)
	merged := map[string]tweet{}
	existing, err := run.Out.Read(ctx, jsonPath)
	if err != nil && !errors.Is(err, plugin.ErrNotExist) {
		return err
	}
	if err == nil {
		var old []json.RawMessage
		if err := json.Unmarshal(existing, &old); err != nil {
			return fmt.Errorf("%s en el NAS no se puede leer: %w", jsonPath, err)
		}
		for _, raw := range old {
			var head struct {
				ID        string `json:"id_str"`
				CreatedAt string `json:"created_at"`
			}
			if json.Unmarshal(raw, &head) == nil && head.ID != "" {
				created, _ := time.Parse(createdLayout, head.CreatedAt)
				merged[head.ID] = tweet{raw: raw, id: head.ID, created: created.UTC()}
			}
		}
	}
	added := 0
	for _, t := range tweets {
		if _, ok := merged[t.id]; !ok {
			added++
		}
		merged[t.id] = t // the newest archive wins: counts and edits are fresher
	}
	list := make([]tweet, 0, len(merged))
	for _, t := range merged {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].created.Equal(list[j].created) {
			return list[i].created.Before(list[j].created)
		}
		return list[i].id < list[j].id
	})

	raws := make([]json.RawMessage, len(list))
	for i, t := range list {
		raws[i] = t.raw
	}
	modTime := list[len(list)-1].created
	if _, err := run.Out.Write(ctx, plugin.File{Path: jsonPath, ModTime: modTime, Data: plugin.JSON(raws), Mutable: true}); err != nil {
		return err
	}
	md := monthMarkdown(username, month, list, media)
	if _, err := run.Out.Write(ctx, plugin.File{Path: strings.TrimSuffix(jsonPath, ".json") + ".md", ModTime: modTime, Data: md, Mutable: true}); err != nil {
		return err
	}
	run.Item(added)
	return nil
}

var monthNames = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

type tweetText struct {
	FullText string `json:"full_text"`
	Text     string `json:"text"`
	ReplyTo  string `json:"in_reply_to_screen_name"`
	Likes    string `json:"favorite_count"`
	Retweets string `json:"retweet_count"`
	Entities struct {
		URLs []struct {
			URL      string `json:"url"`
			Expanded string `json:"expanded_url"`
		} `json:"urls"`
		Media []struct {
			URL string `json:"url"`
		} `json:"media"`
	} `json:"entities"`
}

func monthMarkdown(username, month string, list []tweet, media map[string][]string) []byte {
	var b bytes.Buffer
	var monthNumber int
	fmt.Sscanf(month[5:], "%d", &monthNumber)
	fmt.Fprintf(&b, "# Tuits de @%s: %s de %s\n\n", username, monthNames[monthNumber-1], month[:4])
	for _, t := range list {
		var tt tweetText
		json.Unmarshal(t.raw, &tt)
		text := tt.FullText
		if text == "" {
			text = tt.Text
		}
		// t.co links are short links that will die: show where they went.
		for _, u := range tt.Entities.URLs {
			if u.URL != "" && u.Expanded != "" {
				text = strings.ReplaceAll(text, u.URL, u.Expanded)
			}
		}
		for _, m := range tt.Entities.Media {
			if m.URL != "" {
				text = strings.TrimSpace(strings.ReplaceAll(text, m.URL, ""))
			}
		}
		fmt.Fprintf(&b, "## %s\n\n", t.created.Format("2006-01-02 15:04"))
		if tt.ReplyTo != "" {
			fmt.Fprintf(&b, "_En respuesta a @%s_\n\n", tt.ReplyTo)
		}
		b.WriteString(quote(text))
		b.WriteString("\n\n")
		for _, rel := range media[t.id] {
			// From tweets/AAAA/AAAA-MM.md up to the folder.
			link := "../../" + rel
			if isVideo(rel) {
				fmt.Fprintf(&b, "[vídeo](%s)\n\n", link)
			} else {
				fmt.Fprintf(&b, "![](%s)\n\n", link)
			}
		}
		fmt.Fprintf(&b, "<https://x.com/%s/status/%s>", username, t.id)
		if tt.Likes != "" && tt.Likes != "0" || tt.Retweets != "" && tt.Retweets != "0" {
			fmt.Fprintf(&b, " · %s me gusta · %s retuits", orZero(tt.Likes), orZero(tt.Retweets))
		}
		b.WriteString("\n\n")
	}
	return b.Bytes()
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func isVideo(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".mov", ".m4v", ".webm":
		return true
	}
	return false
}

// quote keeps the line breaks of a tweet in Markdown.
func quote(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ") + "  "
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " ")
}

func writeLikes(ctx context.Context, run *plugin.Run, a *archive) error {
	if len(a.likes) == 0 {
		return nil
	}
	merged := map[string]json.RawMessage{}
	order := []string{}
	add := func(raw json.RawMessage) {
		var head struct {
			ID string `json:"tweetId"`
		}
		if json.Unmarshal(raw, &head) != nil || head.ID == "" {
			return
		}
		if _, ok := merged[head.ID]; !ok {
			order = append(order, head.ID)
		}
		merged[head.ID] = raw
	}
	if existing, err := run.Out.Read(ctx, "me-gusta.json"); err == nil {
		var old []json.RawMessage
		if json.Unmarshal(existing, &old) == nil {
			for _, raw := range old {
				add(raw)
			}
		}
	} else if !errors.Is(err, plugin.ErrNotExist) {
		return err
	}
	for _, raw := range a.likes {
		add(raw)
	}
	sort.Slice(order, func(i, j int) bool {
		if len(order[i]) != len(order[j]) {
			return len(order[i]) > len(order[j]) // ids are numbers: longer is newer
		}
		return order[i] > order[j]
	})
	out := make([]json.RawMessage, len(order))
	var md bytes.Buffer
	fmt.Fprintf(&md, "# «Me gusta» de @%s\n\n", a.username)
	for i, id := range order {
		out[i] = merged[id]
		var like struct {
			Text string `json:"fullText"`
			URL  string `json:"expandedUrl"`
		}
		json.Unmarshal(merged[id], &like)
		fmt.Fprintf(&md, "- %s <%s>\n", strings.Join(strings.Fields(like.Text), " "), like.URL)
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: "me-gusta.json", ModTime: run.Now(), Data: plugin.JSON(out), Mutable: true}); err != nil {
		return err
	}
	_, err := run.Out.Write(ctx, plugin.File{Path: "me-gusta.md", ModTime: run.Now(), Data: md.Bytes(), Mutable: true})
	return err
}
