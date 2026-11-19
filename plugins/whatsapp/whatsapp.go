// Package whatsapp imports the chats exported from the WhatsApp app («Exportar
// chat»), one chat at a time, from Android or iPhone, with or without media.
//
// WhatsApp has no API for a person's own chats, and its export is plain text
// without ids or time zones. Messages are told apart by their time, author
// and text, so successive exports of the same chat, which overlap, merge into
// the same monthly files without repeating anything; what was deleted in the
// phone stays in the copy.
package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/internal/exportkit"
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the WhatsApp chat export importer.
type Plugin struct{}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:        "whatsapp",
		Title:       "WhatsApp (chats exportados)",
		Description: "Importa los chats que exportas desde el móvil: mensajes por meses en JSON y Markdown, con fotos, vídeos, audios y documentos.",
		Help: "WhatsApp exporta los chats de uno en uno, desde el móvil. En Android: abre el chat → ⋮ → Más → Exportar chat. " +
			"En iPhone: abre el chat → toca el nombre arriba → Exportar chat. " +
			"Elige «Incluir archivos» para que vayan también las fotos, vídeos, audios y documentos (sin ellos solo va el texto), y guárdalo o envíatelo al ordenador. " +
			"Deja el ZIP o el .txt («Chat de WhatsApp con Ana.txt», «WhatsApp Chat - Ana.zip») en la carpeta que indiques aquí. " +
			"Puedes volver a exportar el mismo chat cuando quieras: se añade lo nuevo sin repetir lo que ya estaba, y lo que borres en el móvil sigue en la copia.",
		DefaultFolder:     "/Archivo/WhatsApp",
		DefaultEveryHours: 24,
		Fields: []plugin.Field{
			{Name: "path", Label: "Carpeta donde dejas los chats exportados (o un ZIP o .txt)", Type: "path", Required: true, Placeholder: "/home/ana/Descargas/WhatsApp",
				Help: "Una carpeta con ZIP y .txt de chats exportados, uno solo, o un ZIP ya descomprimido."},
		},
	}
}

// Chat files are named after the chat, in the language of the phone.
var chatPrefixes = regexp.MustCompile(`(?i)^(whatsapp chat with |whatsapp chat - |chat de whatsapp con |chat de whatsapp - |conversa do whatsapp com |conversa do whatsapp - |whatsapp-chat mit |discussion whatsapp avec |chat whatsapp con |whatsapp chat met )`)

// browserCopy is the « (1)» a browser adds to a second download of a file.
var browserCopy = regexp.MustCompile(`\s*\(\d+\)$`)

func isChatFile(name string) bool {
	base := path.Base(name)
	if !strings.EqualFold(path.Ext(base), ".txt") {
		return false
	}
	return base == "_chat.txt" || chatPrefixes.MatchString(base)
}

var finder = exportkit.Finder{
	// An unpacked export: iPhone's «_chat.txt», or one chat text with its
	// media and nothing else. A folder with several chats, ZIPs or folders is
	// a drop folder instead.
	IsExport: func(fsys fs.FS) bool {
		entries, _ := fs.ReadDir(fsys, ".")
		texts := 0
		for _, e := range entries {
			switch {
			case !e.IsDir() && e.Name() == "_chat.txt":
				return true
			case e.IsDir(), strings.EqualFold(path.Ext(e.Name()), ".zip"):
				return false
			case isChatFile(e.Name()):
				texts++
			}
		}
		return texts == 1
	},
	Accept: func(name string) bool {
		return strings.EqualFold(filepath.Ext(name), ".zip") || isChatFile(name)
	},
}

// chatName takes «Ana» from «Chat de WhatsApp con Ana.txt», or from the name
// of the ZIP when the text inside is iPhone's «_chat.txt».
func chatName(file, sourceName string) string {
	base := path.Base(file)
	if base == "_chat.txt" {
		base = sourceName
		if dir := path.Dir(file); dir != "." {
			base = path.Base(dir) + ".zip"
		}
	}
	base = strings.TrimSuffix(strings.TrimSuffix(base, filepath.Ext(base)), ".txt")
	base = browserCopy.ReplaceAllString(base, "")
	base = chatPrefixes.ReplaceAllString(base, "")
	return strings.TrimSpace(base)
}

// chats lists the chat texts of an export: the loose .txt itself, or the ones
// in a ZIP or folder (at the top or one folder down).
func chats(s exportkit.Source, fsys fs.FS) []string {
	if !s.Dir && !exportkit.IsZip(s.Path) {
		return []string{filepath.Base(s.Path)}
	}
	out := []string{}
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != "." && (s.Dir || strings.Contains(p, "/") || strings.HasPrefix(d.Name(), "__MACOSX")) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(path.Ext(p), ".txt") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// Validate implements plugin.Plugin.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error {
	list, err := finder.Find(env.Config.Get("path"))
	if err != nil {
		return err
	}
	for _, s := range list {
		fsys, closer, err := s.Open()
		if err != nil {
			return err
		}
		found := false
		for _, file := range chats(s, fsys) {
			if lines, err := readChat(fsys, file); err == nil && len(lines) > 0 {
				found = true
				break
			}
		}
		closer()
		if !found {
			return fmt.Errorf("%s no parece un chat exportado de WhatsApp", s.Name())
		}
	}
	return nil
}

func readChat(fsys fs.FS, file string) ([]*line, error) {
	f, err := fsys.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseText(f)
}

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	return exportkit.Sync(ctx, run, finder, importSource)
}

func importSource(ctx context.Context, run *plugin.Run, s exportkit.Source, _ []exportkit.Source) error {
	fsys, closer, err := s.Open()
	if err != nil {
		return err
	}
	defer closer()
	sourceName := s.Name()
	if s.Dir {
		sourceName += ".zip" // so that chatName strips a single extension
	}
	imported := 0
	for _, file := range chats(s, fsys) {
		if err := ctx.Err(); err != nil {
			return err
		}
		lines, err := readChat(fsys, file)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			run.Logf("%s no tiene mensajes de WhatsApp; se salta", file)
			continue
		}
		if err := importChat(ctx, run, fsys, file, chatName(file, sourceName), lines); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		imported++
	}
	if imported == 0 {
		run.Logf("%s no tiene ningún chat de WhatsApp", s.Name())
	}
	return nil
}

// message is how a message is kept in the JSON of a month.
type message struct {
	ID      string `json:"id"`   // see key
	Date    string `json:"date"` // the wall clock the phone showed, without zone
	Type    string `json:"type"` // «message» or «system»
	Author  string `json:"author,omitempty"`
	Text    string `json:"text,omitempty"`
	Media   string `json:"media,omitempty"`         // the attached file, as WhatsApp named it
	File    string `json:"file,omitempty"`          // where it is, from the chat folder
	Omitted bool   `json:"media_omitted,omitempty"` // exported without «Incluir archivos»
}

const dateLayout = "2006-01-02T15:04:05"

// key tells a message apart without ids: time, author and text, plus which
// repetition it is (two «ok» in the same minute are two messages). Whether
// the media came with the export is left out, so that an export with files
// and one without match.
func key(m message, hasMedia bool, seen map[string]int) string {
	base := m.Date + "\x00" + m.Type + "\x00" + m.Author + "\x00" + m.Text
	if hasMedia {
		base += "\x00media"
	}
	h := plugin.ShortHash(base)
	seen[h]++
	return fmt.Sprintf("%s-%d", h, seen[h])
}

func chatFolder(name string) string {
	slug := plugin.Slug(name, 60)
	if slug == "" {
		slug = "chat-" + plugin.ShortHash(name)
	}
	return "chats/" + slug
}

func importChat(ctx context.Context, run *plugin.Run, fsys fs.FS, file, name string, lines []*line) error {
	folder := chatFolder(name)
	dir := path.Dir(file)
	exists := func(n string) bool {
		clean, ok := exportkit.CleanPath(path.Join(dir, n))
		if !ok {
			return false
		}
		info, err := fs.Stat(fsys, clean)
		return err == nil && !info.IsDir()
	}
	order := dateOrder(lines)
	seen := map[string]int{}
	used := map[string]string{} // stable media name → name in this export
	var records []exportkit.Record
	var last time.Time
	for _, l := range lines {
		when, err := l.when(order)
		if err != nil {
			run.Logf("%s: %v", name, err)
			continue
		}
		m := message{Date: when.Format(dateLayout), Type: "message", Author: l.author, Text: strings.TrimSpace(l.text)}
		if l.system {
			m.Type, m.Author = "system", ""
		}
		var written string
		if m.Type == "message" {
			written, m.Text, m.Omitted = attachment(m.Text, exists)
			if written == "" && !m.Omitted && l.iphone && l.marked && m.Author == name {
				// iPhone writes its notices as messages of the chat itself.
				m.Type, m.Author = "system", ""
			}
		}
		hasMedia := written != "" || m.Omitted
		if written != "" {
			m.Media = iphoneNumber.ReplaceAllString(path.Base(written), "")
			stable := stableName(written, when)
			if other, ok := used[stable]; ok && other != written {
				// Two files of one second with the same name: keep both.
				ext := path.Ext(stable)
				stable = strings.TrimSuffix(stable, ext) + "-" + plugin.ShortHash(written)[:4] + ext
			}
			used[stable] = written
			src, ok := exportkit.CleanPath(path.Join(dir, written))
			if ok {
				rel := "media/" + when.Format("2006-01") + "/" + plugin.SafeName(stable, 120)
				copied, err := exportkit.CopyMedia(ctx, run, fsys, src, folder+"/"+rel, when)
				if err != nil {
					return err
				}
				if copied {
					m.File = rel
				}
			}
		}
		m.ID = key(m, hasMedia, seen)
		raw, _ := json.Marshal(m)
		records = append(records, exportkit.Record{Key: m.ID, Time: when, Raw: raw})
		if when.After(last) {
			last = when
		}
	}
	if len(records) == 0 {
		return nil
	}

	// The text as it came, small and readable on its own, named by its
	// content so that the same export is never written twice.
	original, err := fs.ReadFile(fsys, file)
	if err != nil {
		return err
	}
	originalPath := fmt.Sprintf("%s/originales/hasta-%s-%s.txt", folder, last.Format("2006-01-02"), plugin.ShortHash(string(original)))
	if _, err := run.Out.Write(ctx, plugin.File{Path: originalPath, ModTime: last, Data: original}); err != nil {
		return err
	}

	months, byMonth := exportkit.GroupByMonth(records)
	for _, month := range months {
		list, added, err := exportkit.Merge(ctx, run.Out, exportkit.MonthPath(folder, month), byMonth[month], parseRecord, preferWithFile)
		if err != nil {
			return err
		}
		if err := exportkit.WriteMonth(ctx, run, exportkit.MonthPath(folder, month), list, monthMarkdown(name, month, list)); err != nil {
			return err
		}
		run.Item(added)
	}
	return nil
}

func parseRecord(raw json.RawMessage) (exportkit.Record, bool) {
	var m message
	if json.Unmarshal(raw, &m) != nil || m.ID == "" {
		return exportkit.Record{}, false
	}
	when, err := time.Parse(dateLayout, m.Date)
	if err != nil {
		return exportkit.Record{}, false
	}
	return exportkit.Record{Key: m.ID, Time: when, Raw: raw}, true
}

// preferWithFile keeps the version of a message that has its media: an export
// made without «Incluir archivos» must not hide a photo saved before.
func preferWithFile(old, fresh exportkit.Record) exportkit.Record {
	var o, f message
	json.Unmarshal(old.Raw, &o)
	json.Unmarshal(fresh.Raw, &f)
	if o.File != "" && f.File == "" {
		return old
	}
	return fresh
}

func monthMarkdown(name, month string, list []exportkit.Record) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s: %s\n\n", name, exportkit.MonthTitle(month))
	for _, r := range list {
		var m message
		json.Unmarshal(r.Raw, &m)
		when := r.Time.Format("2006-01-02 15:04")
		if m.Type == "system" {
			fmt.Fprintf(&b, "_%s · %s_\n\n", when, strings.Join(strings.Fields(m.Text), " "))
			continue
		}
		fmt.Fprintf(&b, "**%s** · %s\n\n", m.Author, when)
		if m.Text != "" {
			b.WriteString(exportkit.Quote(m.Text))
			b.WriteString("\n\n")
		}
		switch {
		case m.File != "":
			// From <chat>/AAAA/AAAA-MM.md up to the chat folder.
			b.WriteString(exportkit.MediaLink("../" + m.File))
			b.WriteString("\n\n")
		case m.Media != "" || m.Omitted:
			b.WriteString("_(fichero no incluido en la exportación)_\n\n")
		}
	}
	return b.Bytes()
}
