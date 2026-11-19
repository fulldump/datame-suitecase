// Package telegram imports the export of Telegram Desktop («Exportar datos de
// Telegram», in machine-readable JSON): every chat as monthly JSON and
// Markdown, with its photos, videos, voice notes and files.
//
// Like the X archive, months are merged with what is on the NAS, so a newer
// export adds messages and never removes them.
package telegram

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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the Telegram export importer.
type Plugin struct{}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:        "telegram",
		Title:       "Telegram (exportación de Telegram Desktop)",
		Description: "Importa tus chats exportados: mensajes por meses en JSON y Markdown, con fotos, vídeos, audios y ficheros.",
		Help: "En Telegram Desktop (el programa para ordenador), ve a Ajustes → Avanzado → Exportar datos de Telegram. " +
			"Marca los chats y los tipos de fichero que quieras, elige el formato «JSON legible por máquinas» y exporta. " +
			"Indica aquí la carpeta que crea (DataExport_…), un ZIP de ella, o una carpeta donde vayas dejando exportaciones: cada una nueva añade lo que falte. " +
			"La app del móvil no exporta; hace falta la de ordenador.",
		DefaultFolder:     "/Archivo/Telegram",
		DefaultEveryHours: 24,
		Fields: []plugin.Field{
			{Name: "path", Label: "Carpeta de la exportación (o donde las dejas)", Type: "path", Required: true, Placeholder: "/home/ana/Descargas/Telegram Desktop"},
		},
	}
}

// source is one export: a folder with result.json, or a ZIP with one inside.
type source struct {
	path string
	key  string
	zip  bool
}

func keyOf(name string, info os.FileInfo) string {
	return fmt.Sprintf("%s|%d|%d", name, info.Size(), info.ModTime().Unix())
}

func sources(root string) ([]source, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("no encuentro %s", root)
	}
	if !info.IsDir() {
		return []source{{path: root, key: keyOf(filepath.Base(root), info), zip: true}}, nil
	}
	if result, err := os.Stat(filepath.Join(root, "result.json")); err == nil {
		return []source{{path: root, key: keyOf(filepath.Base(root), result)}}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := []source{}
	for _, entry := range entries {
		full := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			if result, err := os.Stat(filepath.Join(full, "result.json")); err == nil {
				out = append(out, source{path: full, key: keyOf(entry.Name(), result)})
			}
		} else if strings.EqualFold(filepath.Ext(entry.Name()), ".zip") {
			if i, err := entry.Info(); err == nil {
				out = append(out, source{path: full, key: keyOf(entry.Name(), i), zip: true})
			}
		}
	}
	// DataExport_AAAA-MM-DD sorts by date: the oldest first.
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

// open returns the export as a file system rooted where result.json is.
func open(s source) (fs.FS, func() error, error) {
	if !s.zip {
		return os.DirFS(s.path), func() error { return nil }, nil
	}
	r, err := zip.OpenReader(s.path)
	if err != nil {
		return nil, nil, fmt.Errorf("%s no es un ZIP válido", filepath.Base(s.path))
	}
	for _, f := range r.File {
		if path.Base(f.Name) == "result.json" {
			dir := path.Dir(f.Name)
			if dir == "." {
				return r, r.Close, nil
			}
			sub, err := fs.Sub(r, dir)
			if err != nil {
				r.Close()
				return nil, nil, err
			}
			return sub, r.Close, nil
		}
	}
	r.Close()
	return nil, nil, fmt.Errorf("%s no tiene result.json: exporta en formato JSON", filepath.Base(s.path))
}

// Validate implements plugin.Plugin.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error {
	list, err := sources(env.Config.Get("path"))
	if err != nil {
		return err
	}
	for _, s := range list {
		_, closer, err := open(s)
		if err != nil {
			return err
		}
		closer()
	}
	return nil
}

type cursor struct {
	Imported map[string]int64  `json:"imported"`
	Folders  map[string]string `json:"folders"` // chat id → its folder, so a renamed chat stays in place
}

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	var c cursor
	if err := run.Load(&c); err != nil {
		return err
	}
	if c.Imported == nil {
		c.Imported = map[string]int64{}
	}
	if c.Folders == nil {
		c.Folders = map[string]string{}
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
		if err := importSource(ctx, run, &c, s); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(s.path), err)
		}
		c.Imported[s.key] = run.Now().Unix()
		if err := run.Save(c); err != nil {
			return err
		}
	}
	if pending == 0 {
		run.Logf("No hay exportaciones nuevas que importar")
	}
	return nil
}

// ---- The export ----

type chat struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	ID       json.Number       `json:"id"`
	Messages []json.RawMessage `json:"messages"`
}

type export struct {
	chat                                       // an export of one chat
	Chats                struct{ List []chat } `json:"chats"`
	LeftChats            struct{ List []chat } `json:"left_chats"`
	PersonalInformation  json.RawMessage       `json:"personal_information"`
	Contacts             json.RawMessage       `json:"contacts"`
	FrequentContacts     json.RawMessage       `json:"frequent_contacts"`
	ProfilePictures      json.RawMessage       `json:"profile_pictures"`
	ProfilePicturesNewer json.RawMessage       `json:"profile_pictures_v2"`
}

type message struct {
	ID       int64           `json:"id"`
	Type     string          `json:"type"`
	Date     string          `json:"date"`
	Unix     string          `json:"date_unixtime"`
	From     string          `json:"from"`
	Actor    string          `json:"actor"`
	Action   string          `json:"action"`
	Text     json.RawMessage `json:"text"`
	Photo    string          `json:"photo"`
	File     string          `json:"file"`
	Media    string          `json:"media_type"`
	ReplyTo  int64           `json:"reply_to_message_id"`
	Forward  string          `json:"forwarded_from"`
	Edited   string          `json:"edited"`
	Title    string          `json:"title"`
	Poll     json.RawMessage `json:"poll"`
	Location json.RawMessage `json:"location_information"`
}

// when returns the wall-clock time Telegram showed, which is how the export
// writes «date»; date_unixtime is used only when that is missing.
func (m message) when() time.Time {
	if t, err := time.Parse("2006-01-02T15:04:05", m.Date); err == nil {
		return t
	}
	if seconds, err := strconv.ParseInt(m.Unix, 10, 64); err == nil {
		return time.Unix(seconds, 0).UTC()
	}
	return time.Time{}
}

func importSource(ctx context.Context, run *plugin.Run, c *cursor, s source) error {
	fsys, closer, err := open(s)
	if err != nil {
		return err
	}
	defer closer()
	file, err := fsys.Open("result.json")
	if err != nil {
		return err
	}
	var e export
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	err = decoder.Decode(&e)
	file.Close()
	if err != nil {
		return fmt.Errorf("result.json no se puede leer: %w", err)
	}
	chats := append(e.Chats.List, e.LeftChats.List...)
	if e.chat.Messages != nil {
		chats = append(chats, e.chat)
	}
	run.Logf("La exportación tiene %d chats", len(chats))

	// The account sections, as they come.
	for name, raw := range map[string]json.RawMessage{"perfil.json": e.PersonalInformation, "contactos.json": e.Contacts} {
		if len(raw) > 0 && string(raw) != "null" {
			if _, err := run.Out.Write(ctx, plugin.File{Path: name, ModTime: run.Now(), Data: indent(raw), Mutable: true}); err != nil {
				return err
			}
		}
	}

	for _, ch := range chats {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := importChat(ctx, run, c, fsys, ch); err != nil {
			return err
		}
	}
	return nil
}

func indent(raw json.RawMessage) []byte {
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		return raw
	}
	b.WriteByte('\n')
	return b.Bytes()
}

func chatFolder(c *cursor, ch chat) string {
	id := ch.ID.String()
	if id == "" {
		id = "sin-id"
	}
	if folder := c.Folders[id]; folder != "" {
		return folder
	}
	name := plugin.Slug(ch.Name, 50)
	if name == "" {
		name = plugin.Slug(strings.ReplaceAll(ch.Type, "_", " "), 30)
	}
	folder := "chats/" + plugin.SafeName(id, 30)
	if name != "" {
		folder += "-" + name
	}
	c.Folders[id] = folder
	return folder
}

func importChat(ctx context.Context, run *plugin.Run, c *cursor, fsys fs.FS, ch chat) error {
	folder := chatFolder(c, ch)
	byMonth := map[string][]json.RawMessage{}
	media := map[int64]string{} // message id → path relative to the chat folder
	for _, raw := range ch.Messages {
		var m message
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		when := m.when()
		if when.IsZero() {
			continue
		}
		month := when.Format("2006-01")
		byMonth[month] = append(byMonth[month], raw)
		for _, attached := range []string{m.Photo, m.File} {
			if attached == "" || strings.HasPrefix(attached, "(") { // «(File not included…)»
				continue
			}
			rel, err := saveMedia(ctx, run, fsys, folder, month, m.ID, attached, when)
			if err != nil {
				return err
			}
			if rel != "" {
				media[m.ID] = rel
			}
		}
	}
	months := make([]string, 0, len(byMonth))
	for m := range byMonth {
		months = append(months, m)
	}
	sort.Strings(months)
	for _, month := range months {
		if err := writeMonth(ctx, run, folder, ch, month, byMonth[month], media); err != nil {
			return err
		}
	}
	return nil
}

// saveMedia writes an attached file. It is named after the message, which is
// stable, because Telegram numbers files differently in each export.
func saveMedia(ctx context.Context, run *plugin.Run, fsys fs.FS, folder, month string, id int64, attached string, when time.Time) (string, error) {
	name := path.Clean(strings.ReplaceAll(attached, "\\", "/"))
	if strings.HasPrefix(name, "../") || path.IsAbs(name) {
		return "", nil
	}
	info, err := fs.Stat(fsys, name)
	if err != nil {
		run.Logf("Falta %s en la exportación", name)
		return "", nil
	}
	rel := fmt.Sprintf("media/%s/%d-%s", month, id, plugin.SafeName(path.Base(name), 100))
	_, err = run.Out.Write(ctx, plugin.File{Path: folder + "/" + rel, ModTime: when, Size: info.Size(),
		Open: func() (io.ReadCloser, error) { return fsys.Open(name) }})
	return rel, err
}

func writeMonth(ctx context.Context, run *plugin.Run, folder string, ch chat, month string, messages []json.RawMessage, media map[int64]string) error {
	jsonPath := fmt.Sprintf("%s/%s/%s.json", folder, month[:4], month)
	merged := map[int64]json.RawMessage{}
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
			var m message
			if json.Unmarshal(raw, &m) == nil {
				merged[m.ID] = raw
			}
		}
	}
	added := 0
	for _, raw := range messages {
		var m message
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if _, ok := merged[m.ID]; !ok {
			added++
		}
		merged[m.ID] = raw
	}
	ids := make([]int64, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	list := make([]json.RawMessage, len(ids))
	var last time.Time
	for i, id := range ids {
		list[i] = merged[id]
		var m message
		json.Unmarshal(list[i], &m)
		if w := m.when(); w.After(last) {
			last = w
		}
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: jsonPath, ModTime: last, Data: plugin.JSON(list), Mutable: true}); err != nil {
		return err
	}
	md := monthMarkdown(ch, month, list, media)
	if _, err := run.Out.Write(ctx, plugin.File{Path: strings.TrimSuffix(jsonPath, ".json") + ".md", ModTime: last, Data: md, Mutable: true}); err != nil {
		return err
	}
	run.Item(added)
	return nil
}

var monthNames = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

// text renders the «text» of a message, a string or a list of pieces.
func text(raw json.RawMessage) string {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		if json.Unmarshal(part, &plain) == nil {
			b.WriteString(plain)
			continue
		}
		var piece struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Href string `json:"href"`
		}
		json.Unmarshal(part, &piece)
		switch piece.Type {
		case "text_link":
			fmt.Fprintf(&b, "[%s](%s)", piece.Text, piece.Href)
		case "link":
			fmt.Fprintf(&b, "<%s>", piece.Text)
		case "bold":
			fmt.Fprintf(&b, "**%s**", piece.Text)
		case "italic":
			fmt.Fprintf(&b, "_%s_", piece.Text)
		case "code":
			fmt.Fprintf(&b, "`%s`", piece.Text)
		case "pre":
			fmt.Fprintf(&b, "\n```\n%s\n```\n", piece.Text)
		default:
			b.WriteString(piece.Text)
		}
	}
	return b.String()
}

func monthMarkdown(ch chat, month string, list []json.RawMessage, media map[int64]string) []byte {
	var b bytes.Buffer
	var n int
	fmt.Sscanf(month[5:], "%d", &n)
	title := ch.Name
	if title == "" {
		title = "Mensajes guardados"
	}
	fmt.Fprintf(&b, "# %s: %s de %s\n\n", title, monthNames[n-1], month[:4])
	for _, raw := range list {
		var m message
		json.Unmarshal(raw, &m)
		when := m.when().Format("2006-01-02 15:04")
		if m.Type == "service" {
			fmt.Fprintf(&b, "_%s · %s: %s_\n\n", when, m.Actor, strings.ReplaceAll(m.Action, "_", " "))
			continue
		}
		fmt.Fprintf(&b, "**%s** · %s", m.From, when)
		if m.Forward != "" {
			fmt.Fprintf(&b, " · reenviado de %s", m.Forward)
		}
		if m.ReplyTo != 0 {
			fmt.Fprintf(&b, " · en respuesta al mensaje %d", m.ReplyTo)
		}
		if m.Edited != "" {
			b.WriteString(" · editado")
		}
		b.WriteString("\n\n")
		if t := strings.TrimSpace(text(m.Text)); t != "" {
			lines := strings.Split(t, "\n")
			for i := range lines {
				lines[i] = strings.TrimRight(lines[i], " ") + "  "
			}
			b.WriteString(strings.TrimRight(strings.Join(lines, "\n"), " "))
			b.WriteString("\n\n")
		}
		if rel, ok := media[m.ID]; ok {
			// From <chat>/AAAA/AAAA-MM.md up to the chat folder.
			link := "../" + rel
			if m.Photo != "" {
				fmt.Fprintf(&b, "![](%s)\n\n", link)
			} else {
				label := m.Media
				if label == "" {
					label = "fichero"
				}
				fmt.Fprintf(&b, "[%s: %s](%s)\n\n", strings.ReplaceAll(label, "_", " "), path.Base(rel), link)
			}
		} else if m.Photo != "" || m.File != "" {
			b.WriteString("_(fichero no incluido en la exportación)_\n\n")
		}
	}
	return b.Bytes()
}
