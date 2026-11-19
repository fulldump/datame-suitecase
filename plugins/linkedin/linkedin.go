// Package linkedin keeps a copy of a LinkedIn account. On its own, with the
// session cookie, through the internal API of its web (see live.go); and from
// the copy of the data that LinkedIn gives to each member («Obtener una copia
// de tus datos»), a ZIP of CSV files, which brings a few things more (emails of
// the contacts, invitations) and is the fallback when the web changes.
//
// The CSVs are kept as they came, and turned into things to read: the
// profile as a CV, the contacts as a table, messages by conversation and
// month, and posts by month, merged with what is on the NAS so that a newer
// copy adds and never removes.
package linkedin

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/internal/exportkit"
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the LinkedIn data copy importer.
type Plugin struct{}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:        "linkedin",
		Title:       "LinkedIn",
		Description: "Copia tu cuenta de LinkedIn sola, cada día: tu perfil como un currículum, tus contactos, tus mensajes por conversación y tus publicaciones por meses.",
		Help: "LinkedIn no tiene una API para leer tus propios datos, así que se usa la de su web con tu sesión: la cookie «li_at». " +
			"Para copiarla: entra en linkedin.com en el navegador, abre las herramientas de desarrollo (F12) → Aplicación (en Firefox, Almacenamiento) → Cookies → https://www.linkedin.com, y copia el valor de «li_at». " +
			"Dura meses; si cierras la sesión en el navegador deja de valer y habrá que copiarla otra vez. Se guarda cifrada. " +
			"Va despacio, una vez al día, y se para si LinkedIn pide una comprobación. Aun así, las condiciones de LinkedIn no permiten el acceso automático: úsalo con tu cuenta y bajo tu responsabilidad. " +
			"Opcional: la copia de tus datos (Yo → Configuración y privacidad → Privacidad de datos → Obtener una copia de tus datos, «Quiero todo») trae además los correos de los contactos y las invitaciones. Si dejas los ZIP en una carpeta, también se importan. " +
			"Lo que borres en LinkedIn sigue en el NAS.",
		DefaultFolder:     "/Archivo/LinkedIn",
		DefaultEveryHours: 24,
		Fields: []plugin.Field{
			{Name: "li_at", Label: "Cookie «li_at» de tu sesión", Type: "password", Secret: true, Help: "Ver cómo copiarla en la ayuda de arriba."},
			{Name: "jsessionid", Label: "Cookie «JSESSIONID» (opcional)", Type: "password", Secret: true, Help: "Solo si sin ella no funciona: se obtiene sola."},
			{Name: "path", Label: "Carpeta donde dejas los ZIP de la copia de tus datos (opcional)", Type: "path", Placeholder: "/home/ana/Descargas/LinkedIn"},
			{Name: "base", Label: "Dirección de LinkedIn", Type: "url", Default: defaultBase, Help: "No hace falta cambiarla."},
		},
	}
}

// csvFiles maps the lower-case name of each CSV of an export to its path:
// LinkedIn has written both «Messages.csv» and «messages.csv».
func csvFiles(fsys fs.FS) map[string]string {
	out := map[string]string{}
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != "." && (strings.Contains(p, "/") || strings.HasPrefix(d.Name(), "__MACOSX")) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(path.Ext(p), ".csv") {
			if _, ok := out[strings.ToLower(path.Base(p))]; !ok || !strings.Contains(p, "/") {
				out[strings.ToLower(path.Base(p))] = p
			}
		}
		return nil
	})
	return out
}

var markers = []string{"connections.csv", "profile.csv", "messages.csv", "positions.csv", "shares.csv"}

// isExport looks only at the top of the folder: a drop folder holding an
// unpacked copy one level down is not a copy itself.
func isExport(fsys fs.FS) bool {
	entries, _ := fs.ReadDir(fsys, ".")
	for _, e := range entries {
		for _, m := range markers {
			if !e.IsDir() && strings.EqualFold(e.Name(), m) {
				return true
			}
		}
	}
	return false
}

var finder = exportkit.Finder{IsExport: isExport}

// Validate implements plugin.Plugin.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error {
	if env.Config.Get("li_at") == "" && env.Config.Get("path") == "" {
		return errors.New("pon la cookie «li_at» para que la copia sea automática (o, al menos, la carpeta de los ZIP)")
	}
	if env.Config.Get("li_at") != "" {
		v, err := newVoyager(env)
		if err != nil {
			return err
		}
		if err := v.start(ctx); err != nil {
			return err
		}
		if _, err := v.get(ctx, "/me"); err != nil {
			return err
		}
	}
	if env.Config.Get("path") == "" {
		return nil
	}
	list, err := finder.Find(env.Config.Get("path"))
	if err != nil {
		return err
	}
	for _, s := range list {
		fsys, closer, err := s.Open()
		if err != nil {
			return err
		}
		ok := isExport(fsys)
		closer()
		if !ok {
			return fmt.Errorf("%s no parece una copia de datos de LinkedIn", s.Name())
		}
	}
	return nil
}

// cursor keeps the state of both ways apart: the export importer saves its
// own cursor whole, and must not wipe the one of the live copy.
type cursor struct {
	Export json.RawMessage `json:"export,omitempty"`
	Live   liveCursor      `json:"live"`
}

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	c := cursor{}
	if raw := run.Cursor(); len(raw) > 0 {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return err
		}
		if _, old := probe["imported"]; old {
			c.Export = raw // from before the live copy: all of it was the importer's
		} else if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
	}
	save := func() error { return run.Save(c) }

	var liveErr error
	if run.Config.Get("li_at") != "" {
		if liveErr = syncLive(ctx, run, &c.Live, save); liveErr != nil {
			run.Logf("Copia automática: %v", liveErr)
		}
	}
	if run.Config.Get("path") != "" {
		// The importer, with a cursor of its own inside this one.
		sub := plugin.NewRun(run.Env, run.Out, c.Export, func(data json.RawMessage) error {
			c.Export = data
			return save()
		}, run.Logf)
		err := exportkit.Sync(ctx, sub, finder, importSource)
		run.Item(sub.Items())
		if err != nil {
			return err
		}
	}
	return liveErr
}

// ---- Reading the CSVs ----

type row map[string]string

// get returns the first of several column names that has a value: the names
// change between versions of the export («Started On», «Start Date»).
func (r row) get(names ...string) string {
	for _, n := range names {
		for k, v := range r {
			if strings.EqualFold(k, n) && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// readCSV reads a CSV of the export. Some start with notes before the table
// («Notes:», a paragraph and a blank line), and some with a byte order mark.
func readCSV(fsys fs.FS, name string) ([]string, []row, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, nil, err
	}
	return parseCSV(name, data)
}

func parseCSV(name string, data []byte) ([]string, []row, error) {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if bytes.HasPrefix(data, []byte("Notes:")) {
		if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
			data = data[i+2:]
		}
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	header, err := r.Read()
	if err == io.EOF {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	for i := range header {
		header[i] = strings.TrimSpace(header[i])
	}
	var rows []row
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		values := row{}
		for i, h := range header {
			if i < len(record) {
				values[h] = record[i]
			}
		}
		rows = append(rows, values)
	}
	return header, rows, nil
}

// LinkedIn writes dates in several ways, depending on the file and the year.
var layouts = []string{
	"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02",
	"1/2/06, 3:04 PM", "01/02/2006, 15:04", "02 Jan 2006", "2 Jan 2006", "Jan 2006", "2006",
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func importSource(ctx context.Context, run *plugin.Run, s exportkit.Source, _ []exportkit.Source) error {
	fsys, closer, err := s.Open()
	if err != nil {
		return err
	}
	defer closer()
	files := csvFiles(fsys)
	read := func(name string) ([]string, []row, error) {
		p, ok := files[name]
		if !ok {
			return nil, nil, nil
		}
		return readCSV(fsys, p)
	}

	// The CSVs as they came, under the name of the export: a newer copy goes
	// beside, never over, an older one.
	export := strings.TrimSuffix(s.Name(), path.Ext(s.Name()))
	if s.Dir {
		export = s.Name()
	}
	export = plugin.SafeName(export, 80)
	names := make([]string, 0, len(files))
	for _, p := range files {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		rel := "originales/" + export
		for _, part := range strings.Split(p, "/") {
			rel += "/" + plugin.SafeName(part, 120)
		}
		if _, err := exportkit.CopyMedia(ctx, run, fsys, p, rel, time.Time{}); err != nil {
			return err
		}
	}

	_, profile, err := read("profile.csv")
	if err != nil {
		return err
	}
	me := ""
	if len(profile) > 0 {
		me = strings.TrimSpace(profile[0].get("First Name") + " " + profile[0].get("Last Name"))
		if err := writeProfile(ctx, run, profile[0], read); err != nil {
			return err
		}
	}
	if _, rows, err := read("connections.csv"); err != nil {
		return err
	} else if len(rows) > 0 {
		if err := writeContacts(ctx, run, rows); err != nil {
			return err
		}
	}
	if _, rows, err := read("messages.csv"); err != nil {
		return err
	} else if len(rows) > 0 {
		if err := writeMessages(ctx, run, rows, me); err != nil {
			return err
		}
	}
	if _, rows, err := read("shares.csv"); err != nil {
		return err
	} else if len(rows) > 0 {
		if err := writePosts(ctx, run, rows); err != nil {
			return err
		}
	}
	return nil
}

// ---- Profile ----

func writeProfile(ctx context.Context, run *plugin.Run, p row, read func(string) ([]string, []row, error)) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s %s\n\n", p.get("First Name"), p.get("Last Name"))
	if h := p.get("Headline"); h != "" {
		fmt.Fprintf(&b, "**%s**\n\n", h)
	}
	details := []string{}
	for _, d := range []struct{ label, value string }{
		{"Ubicación", p.get("Geo Location", "Location")}, {"Sector", p.get("Industry")},
		{"Webs", p.get("Websites")}, {"Twitter", p.get("Twitter Handles")},
	} {
		if d.value != "" {
			details = append(details, d.label+": "+d.value)
		}
	}
	if len(details) > 0 {
		b.WriteString(strings.Join(details, " · ") + "\n\n")
	}
	if s := p.get("Summary"); s != "" {
		b.WriteString("## Extracto\n\n" + exportkit.Quote(s) + "\n\n")
	}

	section := func(file, title string, item func(r row)) error {
		_, rows, err := read(file)
		if err != nil || len(rows) == 0 {
			return err
		}
		b.WriteString("## " + title + "\n\n")
		for _, r := range rows {
			item(r)
		}
		return nil
	}
	period := func(r row, start, end []string) string {
		from, to := r.get(start...), r.get(end...)
		switch {
		case from == "" && to == "":
			return ""
		case to == "":
			return from + " – actualidad"
		}
		return from + " – " + to
	}
	err := section("positions.csv", "Experiencia", func(r row) {
		fmt.Fprintf(&b, "### %s — %s\n\n", r.get("Title"), r.get("Company Name", "Company"))
		meta := []string{}
		if p := period(r, []string{"Started On", "Start Date"}, []string{"Finished On", "End Date"}); p != "" {
			meta = append(meta, p)
		}
		if l := r.get("Location"); l != "" {
			meta = append(meta, l)
		}
		if len(meta) > 0 {
			fmt.Fprintf(&b, "_%s_\n\n", strings.Join(meta, " · "))
		}
		if d := r.get("Description"); d != "" {
			b.WriteString(exportkit.Quote(d) + "\n\n")
		}
	})
	if err != nil {
		return err
	}
	err = section("education.csv", "Formación", func(r row) {
		title := r.get("School Name")
		if d := r.get("Degree Name"); d != "" {
			title += " — " + d
		}
		fmt.Fprintf(&b, "### %s\n\n", title)
		if p := period(r, []string{"Start Date", "Started On"}, []string{"End Date", "Finished On"}); p != "" {
			fmt.Fprintf(&b, "_%s_\n\n", p)
		}
		for _, k := range []string{"Notes", "Activities"} {
			if v := r.get(k); v != "" {
				b.WriteString(exportkit.Quote(v) + "\n\n")
			}
		}
	})
	if err != nil {
		return err
	}
	for _, list := range []struct{ file, title string }{{"skills.csv", "Aptitudes"}, {"languages.csv", "Idiomas"}} {
		_, rows, err := read(list.file)
		if err != nil {
			return err
		}
		names := []string{}
		for _, r := range rows {
			name := r.get("Name")
			if level := r.get("Proficiency"); level != "" {
				name += " (" + level + ")"
			}
			if name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			b.WriteString("## " + list.title + "\n\n" + strings.Join(names, " · ") + "\n\n")
		}
	}
	err = section("certifications.csv", "Certificaciones", func(r row) {
		line := "- " + r.get("Name")
		if a := r.get("Authority"); a != "" {
			line += ", " + a
		}
		if d := r.get("Started On"); d != "" {
			line += " (" + d + ")"
		}
		if u := r.get("Url"); u != "" {
			line += " <" + u + ">"
		}
		b.WriteString(line + "\n")
	})
	if err != nil {
		return err
	}
	_, err = run.Out.Write(ctx, plugin.File{Path: "perfil.md", ModTime: run.Now(), Data: bytes.TrimRight(b.Bytes(), "\n"), Mutable: true})
	return err
}

// ---- Contacts ----

var contactColumns = []string{"First Name", "Last Name", "URL", "Email Address", "Company", "Position", "Connected On"}

func contactKey(r row) string {
	if u := strings.ToLower(strings.TrimRight(r.get("URL"), "/")); u != "" {
		return u
	}
	return strings.ToLower(r.get("First Name") + " " + r.get("Last Name"))
}

// writeContacts merges the contacts with the table on the NAS: a contact
// removed in LinkedIn stays, and the newest data of each one wins.
func writeContacts(ctx context.Context, run *plugin.Run, rows []row) error {
	columns := append([]string{}, contactColumns...)
	addColumns := func(header []string) {
		for _, h := range header {
			found := false
			for _, c := range columns {
				found = found || strings.EqualFold(c, h)
			}
			if !found {
				columns = append(columns, h)
			}
		}
	}
	merged := map[string]row{}
	if existing, err := run.Out.Read(ctx, "contactos.csv"); err == nil {
		header, old, err := parseCSV("contactos.csv", existing)
		if err != nil {
			return fmt.Errorf("contactos.csv en el NAS no se puede leer: %w", err)
		}
		addColumns(header)
		for _, r := range old {
			merged[contactKey(r)] = r
		}
	} else if !errors.Is(err, plugin.ErrNotExist) {
		return err
	}
	added := 0
	for _, r := range rows {
		for k := range r {
			addColumns([]string{k})
		}
		k := contactKey(r)
		if strings.TrimSpace(k) == "" {
			continue
		}
		if _, ok := merged[k]; !ok {
			added++
		}
		merged[k] = r
	}
	list := make([]row, 0, len(merged))
	for _, r := range merged {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := parseDate(list[i].get("Connected On")), parseDate(list[j].get("Connected On"))
		if !a.Equal(b) {
			return a.After(b)
		}
		return contactKey(list[i]) < contactKey(list[j])
	})
	table := make([][]string, len(list))
	var md bytes.Buffer
	fmt.Fprintf(&md, "# Contactos de LinkedIn\n\n%d contactos, del más reciente al más antiguo.\n\n", len(list))
	for i, r := range list {
		table[i] = make([]string, len(columns))
		for j, c := range columns {
			table[i][j] = r.get(c)
		}
		name := strings.TrimSpace(r.get("First Name") + " " + r.get("Last Name"))
		if u := r.get("URL"); u != "" {
			name = fmt.Sprintf("[%s](%s)", name, u)
		}
		line := "- " + name
		switch p, c := r.get("Position"), r.get("Company"); {
		case p != "" && c != "":
			line += " — " + p + " en " + c
		case p != "" || c != "":
			line += " — " + p + c
		}
		if e := r.get("Email Address"); e != "" {
			line += " · " + e
		}
		if d := parseDate(r.get("Connected On")); !d.IsZero() {
			line += " · desde " + d.Format("2006-01-02")
		}
		md.WriteString(line + "\n")
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: "contactos.csv", ModTime: run.Now(), Data: exportkit.CSV(columns, table), Mutable: true}); err != nil {
		return err
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: "contactos.md", ModTime: run.Now(), Data: md.Bytes(), Mutable: true}); err != nil {
		return err
	}
	run.Item(added)
	return nil
}

// ---- Messages ----

func rowRecord(r row, key string, when time.Time) exportkit.Record {
	raw, _ := json.Marshal(r) // a map: keys in order, the same row gives the same bytes
	return exportkit.Record{Key: key, Time: when, Raw: raw}
}

func messageKey(r row) string {
	return plugin.ShortHash(strings.Join([]string{r.get("CONVERSATION ID"), r.get("DATE"), r.get("FROM"), r.get("CONTENT")}, "\x00"))
}

func parseRow(key func(row) string, date ...string) func(json.RawMessage) (exportkit.Record, bool) {
	return func(raw json.RawMessage) (exportkit.Record, bool) {
		var r row
		if json.Unmarshal(raw, &r) != nil {
			return exportkit.Record{}, false
		}
		return exportkit.Record{Key: key(r), Time: parseDate(r.get(date...)), Raw: raw}, true
	}
}

// conversationFolder names a conversation after the other people in its
// first message, plus a piece of its id, which is what keeps it stable.
func conversationFolder(id string, first row, me string) string {
	slug := plugin.Slug(conversationTitle(first, me), 50)
	if slug == "" {
		slug = "conversacion"
	}
	return "mensajes/" + slug + "-" + plugin.ShortHash(id)[:6]
}

func writeMessages(ctx context.Context, run *plugin.Run, rows []row, me string) error {
	byConversation := map[string][]exportkit.Record{}
	first := map[string]row{}
	for _, r := range rows {
		when := parseDate(r.get("DATE"))
		if when.IsZero() {
			continue
		}
		id := r.get("CONVERSATION ID")
		if f, ok := first[id]; !ok || when.Before(parseDate(f.get("DATE"))) {
			first[id] = r
		}
		byConversation[id] = append(byConversation[id], rowRecord(r, messageKey(r), when))
	}
	ids := make([]string, 0, len(byConversation))
	for id := range byConversation {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		folder := conversationFolder(id, first[id], me)
		title := conversationTitle(first[id], me)
		months, byMonth := exportkit.GroupByMonth(byConversation[id])
		for _, month := range months {
			jsonPath := exportkit.MonthPath(folder, month)
			list, added, err := exportkit.Merge(ctx, run.Out, jsonPath, byMonth[month], parseRow(messageKey, "DATE"), nil)
			if err != nil {
				return err
			}
			if err := exportkit.WriteMonth(ctx, run, jsonPath, list, messagesMarkdown(title, month, list)); err != nil {
				return err
			}
			run.Item(added)
		}
	}
	return nil
}

// conversationTitle is the title LinkedIn gave, or the other people.
func conversationTitle(first row, me string) string {
	if t := first.get("CONVERSATION TITLE"); t != "" {
		return t
	}
	people := []string{}
	for _, p := range append([]string{first.get("FROM")}, strings.Split(first.get("TO"), ",")...) {
		if p = strings.TrimSpace(p); p != "" && !strings.EqualFold(p, me) {
			people = append(people, p)
		}
	}
	if len(people) == 0 {
		return "Conversación"
	}
	return strings.Join(people, ", ")
}

func messagesMarkdown(title, month string, list []exportkit.Record) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s: %s\n\n", title, exportkit.MonthTitle(month))
	for _, rec := range list {
		var r row
		json.Unmarshal(rec.Raw, &r)
		fmt.Fprintf(&b, "**%s** · %s UTC\n\n", r.get("FROM"), rec.Time.Format("2006-01-02 15:04"))
		if s := r.get("SUBJECT"); s != "" {
			fmt.Fprintf(&b, "_%s_\n\n", s)
		}
		if c := r.get("CONTENT"); c != "" {
			b.WriteString(exportkit.Quote(c) + "\n\n")
		}
		if a := r.get("ATTACHMENTS"); a != "" {
			fmt.Fprintf(&b, "Adjuntos: %s\n\n", a)
		}
	}
	return b.Bytes()
}

// ---- Posts ----

func postKey(r row) string {
	if l := r.get("ShareLink"); l != "" {
		return l
	}
	return plugin.ShortHash(r.get("Date") + "\x00" + r.get("ShareCommentary"))
}

func writePosts(ctx context.Context, run *plugin.Run, rows []row) error {
	var records []exportkit.Record
	for _, r := range rows {
		when := parseDate(r.get("Date"))
		if when.IsZero() {
			continue
		}
		records = append(records, rowRecord(r, postKey(r), when))
	}
	months, byMonth := exportkit.GroupByMonth(records)
	for _, month := range months {
		jsonPath := exportkit.MonthPath("publicaciones", month)
		list, added, err := exportkit.Merge(ctx, run.Out, jsonPath, byMonth[month], parseRow(postKey, "Date"), nil)
		if err != nil {
			return err
		}
		var b bytes.Buffer
		fmt.Fprintf(&b, "# Publicaciones de LinkedIn: %s\n\n", exportkit.MonthTitle(month))
		for _, rec := range list {
			var r row
			json.Unmarshal(rec.Raw, &r)
			fmt.Fprintf(&b, "## %s UTC\n\n", rec.Time.Format("2006-01-02 15:04"))
			if c := r.get("ShareCommentary"); c != "" {
				b.WriteString(exportkit.Quote(c) + "\n\n")
			}
			for _, k := range []string{"SharedUrl", "MediaUrl", "ShareLink"} {
				if u := r.get(k); u != "" {
					fmt.Fprintf(&b, "<%s>\n\n", u)
				}
			}
		}
		if err := exportkit.WriteMonth(ctx, run, jsonPath, list, b.Bytes()); err != nil {
			return err
		}
		run.Item(added)
	}
	return nil
}
