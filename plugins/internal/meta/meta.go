package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/internal/exportkit"
)

// Rule says what to do with the JSON files whose path inside the export
// matches. Meta moves files between folders from one year to the next
// («content/» became «your_instagram_activity/content/»), so rules look at
// the end of the path, not at all of it.
type Rule struct {
	Match *regexp.Regexp
	// Posts writes the items by month in this folder, with this title.
	Posts, Title string
	// List merges the items in <List>.json and <List>.csv.
	List string
	// Album writes the photos of the file in albumes/<name>/; Title is the
	// name when the file has none.
	Album bool
}

// Service is one Meta product: Instagram or Facebook.
type Service struct {
	Name  string
	Rules []Rule
}

// Messages are the same in both: messages/<box>/<thread>/message_N.json.
var messageFile = regexp.MustCompile(`(^|/)messages/[^/]+/([^/]+)/message_\d+\.json$`)

// The top folders of an unpacked export, of one year or another.
var topFolders = []string{"your_instagram_activity", "your_facebook_activity", "personal_information", "connections",
	"logged_information", "security_and_login_information", "preferences", "ads_information", "messages"}

// Finder finds Meta exports: ZIPs, or unpacked folders.
func (s Service) Finder() exportkit.Finder {
	return exportkit.Finder{IsExport: func(fsys fs.FS) bool {
		for _, name := range topFolders {
			if info, err := fs.Stat(fsys, name); err == nil && info.IsDir() {
				return true
			}
		}
		return false
	}}
}

// jsonFiles lists the JSON files of an export, and whether it has HTML ones
// (the export made in the wrong format).
func jsonFiles(fsys fs.FS) ([]string, bool) {
	out := []string{}
	html := false
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), "__MACOSX") {
				return fs.SkipDir
			}
			return nil
		}
		switch strings.ToLower(path.Ext(p)) {
		case ".json":
			out = append(out, p)
		case ".html":
			html = true
		}
		return nil
	})
	sort.Strings(out)
	return out, html
}

func (s Service) rule(p string) (Rule, bool) {
	for _, r := range s.Rules {
		if r.Match.MatchString(p) {
			return r, true
		}
	}
	return Rule{}, false
}

// Validate checks that each export is a JSON one of this service.
func (s Service) Validate(ctx context.Context, env plugin.Env) error {
	list, err := s.Finder().Find(env.Config.Get("path"))
	if err != nil {
		return err
	}
	for _, src := range list {
		fsys, closer, err := src.Open()
		if err != nil {
			return err
		}
		files, html := jsonFiles(fsys)
		// The second and later ZIPs of a big export may have only photos.
		mediaOnly := len(files) == 0 && !html && s.Finder().IsExport(fsys)
		closer()
		known := false
		for _, p := range files {
			if _, ok := s.rule(p); ok || messageFile.MatchString(p) {
				known = true
				break
			}
		}
		if !known && html {
			return fmt.Errorf("%s está en formato HTML: pide otra exportación eligiendo el formato JSON", src.Name())
		}
		if !known && !mediaOnly {
			return fmt.Errorf("%s no parece una exportación de %s en JSON", src.Name(), s.Name)
		}
	}
	return nil
}

// Sync imports the exports not imported yet.
func (s Service) Sync(ctx context.Context, run *plugin.Run) error {
	return exportkit.Sync(ctx, run, s.Finder(), s.importSource)
}

// export is one export opened, with the others of the folder behind it to
// find the media of a big export split in several ZIPs.
type export struct {
	media exportkit.Union
}

func (s Service) importSource(ctx context.Context, run *plugin.Run, src exportkit.Source, all []exportkit.Source) error {
	fsys, closer, err := src.Open()
	if err != nil {
		return err
	}
	defer closer()
	e := export{media: exportkit.Union{fsys}}
	for _, other := range all {
		if other.Key == src.Key {
			continue
		}
		if ofs, oclose, err := other.Open(); err == nil {
			defer oclose()
			e.media = append(e.media, ofs)
		}
	}

	files, _ := jsonFiles(fsys)
	posts := map[Rule][]any{}
	lists := map[string][]any{}
	threads := map[string][]any{} // thread folder → its files
	threadOrder := []string{}
	for _, p := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		v, err := Decode(data)
		if err != nil {
			run.Logf("%s no se puede leer: %v", p, err)
			continue
		}
		if m := messageFile.FindStringSubmatch(p); m != nil {
			if _, ok := threads[m[2]]; !ok {
				threadOrder = append(threadOrder, m[2])
			}
			threads[m[2]] = append(threads[m[2]], v)
			continue
		}
		r, ok := s.rule(p)
		switch {
		case !ok:
			// Everything else as it came, with the text repaired: what is
			// not understood today is still there to read.
			if _, err := run.Out.Write(ctx, plugin.File{Path: "otros/" + p, ModTime: run.Now(), Data: plugin.JSON(v), Mutable: true}); err != nil {
				return err
			}
		case r.Posts != "":
			posts[r] = append(posts[r], Records(v)...)
		case r.List != "":
			lists[r.List] = append(lists[r.List], Records(v)...)
		case r.Album:
			if err := writeAlbum(ctx, run, e, r, v); err != nil {
				return err
			}
		}
	}

	rules := make([]Rule, 0, len(posts))
	for r := range posts {
		rules = append(rules, r)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Posts < rules[j].Posts })
	for _, r := range rules {
		if err := writePosts(ctx, run, e, r, posts[r]); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(lists))
	for name := range lists {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeList(ctx, run, name, lists[name]); err != nil {
			return err
		}
	}
	for _, thread := range threadOrder {
		if err := writeThread(ctx, run, e, thread, threads[thread]); err != nil {
			return err
		}
	}
	return nil
}

// copyMedia writes the files an item points to under dir, noting where
// inside the item. It is named as in the export: Meta names files by their
// own id.
func copyMedia(ctx context.Context, run *plugin.Run, e export, item any, dir string, when time.Time) error {
	for _, m := range mediaObjects(item) {
		name, ok := exportkit.CleanPath(str(m["uri"]))
		if !ok {
			continue
		}
		rel := dir + "/" + plugin.SafeName(path.Base(name), 120)
		copied, err := exportkit.CopyMedia(ctx, run, e.media, name, rel, when)
		if err != nil {
			return err
		}
		if copied {
			m[localKey] = rel
		}
	}
	return nil
}

func record(v any, key string, when time.Time) exportkit.Record {
	raw, _ := json.Marshal(v)
	return exportkit.Record{Key: key, Time: when, Raw: raw}
}

// preferLocal keeps the copy of an item that has its files on the NAS.
func preferLocal(old, fresh exportkit.Record) exportkit.Record {
	var o, f any
	json.Unmarshal(old.Raw, &o)
	json.Unmarshal(fresh.Raw, &f)
	if hasLocal(o) > hasLocal(f) {
		return old
	}
	return fresh
}

func parser(key func(v any, when time.Time) string) func(json.RawMessage) (exportkit.Record, bool) {
	return func(raw json.RawMessage) (exportkit.Record, bool) {
		v, err := Decode(raw)
		if err != nil {
			return exportkit.Record{}, false
		}
		when := TimeOf(v)
		return exportkit.Record{Key: key(v, when), Time: when, Raw: raw}, true
	}
}

// ---- Posts, stories, reels ----

func postKey(v any, when time.Time) string { return keyOf(v, when, "") }

func writePosts(ctx context.Context, run *plugin.Run, e export, r Rule, items []any) error {
	var records []exportkit.Record
	for _, item := range items {
		when := TimeOf(item)
		if when.IsZero() {
			continue
		}
		key := postKey(item, when)
		if err := copyMedia(ctx, run, e, item, fmt.Sprintf("media/%s/%s", when.Format("2006"), when.Format("2006-01")), when); err != nil {
			return err
		}
		records = append(records, record(item, key, when))
	}
	months, byMonth := exportkit.GroupByMonth(records)
	for _, month := range months {
		jsonPath := exportkit.MonthPath(r.Posts, month)
		list, added, err := exportkit.Merge(ctx, run.Out, jsonPath, byMonth[month], parser(postKey), preferLocal)
		if err != nil {
			return err
		}
		if err := exportkit.WriteMonth(ctx, run, jsonPath, list, postsMarkdown(r.Title, month, list, up(jsonPath))); err != nil {
			return err
		}
		run.Item(added)
	}
	return nil
}

// up is the way from the folder of a file to the root of the integration.
func up(rel string) string {
	return strings.Repeat("../", strings.Count(rel, "/"))
}

func mediaMarkdown(b *bytes.Buffer, v any, back string) {
	for _, m := range mediaObjects(v) {
		if local := str(m[localKey]); local != "" {
			b.WriteString(exportkit.MediaLink(back + local))
			b.WriteString("\n\n")
		} else {
			fmt.Fprintf(b, "_(%s no está en la exportación)_\n\n", baseName(str(m["uri"])))
		}
	}
}

func postsMarkdown(title, month string, list []exportkit.Record, back string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s: %s\n\n", title, exportkit.MonthTitle(month))
	for _, r := range list {
		v, _ := Decode(r.Raw)
		text, header := textOf(v)
		fmt.Fprintf(&b, "## %s UTC\n\n", r.Time.Format("2006-01-02 15:04"))
		if header != "" {
			fmt.Fprintf(&b, "_%s_\n\n", header)
		}
		if text != "" {
			b.WriteString(exportkit.Quote(text))
			b.WriteString("\n\n")
		}
		mediaMarkdown(&b, v, back)
		for _, u := range links(v) {
			fmt.Fprintf(&b, "<%s>\n\n", u)
		}
	}
	return b.Bytes()
}

// ---- Lists ----

func listKey(v any, _ time.Time) string {
	data, _ := json.Marshal(v) // maps marshal with sorted keys: the same item gives the same key
	return plugin.ShortHash(string(data))
}

func writeList(ctx context.Context, run *plugin.Run, name string, items []any) error {
	records := make([]exportkit.Record, 0, len(items))
	for _, item := range items {
		records = append(records, record(item, listKey(item, time.Time{}), TimeOf(item)))
	}
	list, added, err := exportkit.Merge(ctx, run.Out, name+".json", records, parser(listKey), nil)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: name + ".json", ModTime: run.Now(), Data: plugin.JSON(exportkit.Raws(list)), Mutable: true}); err != nil {
		return err
	}
	header, rows := table(list)
	if _, err := run.Out.Write(ctx, plugin.File{Path: name + ".csv", ModTime: run.Now(), Data: exportkit.CSV(header, rows), Mutable: true}); err != nil {
		return err
	}
	run.Item(added)
	return nil
}

// table flattens the items of a list into columns a spreadsheet can show.
// The wrappers Meta puts everywhere («string_list_data», «data») are left
// out of the column names, and times are written as dates.
func table(list []exportkit.Record) ([]string, [][]string) {
	columns := []string{}
	known := map[string]bool{}
	rows := []map[string]string{}
	for _, r := range list {
		v, _ := Decode(r.Raw)
		row := map[string]string{}
		flatten(v, nil, row, func(c string) {
			if !known[c] {
				known[c] = true
				columns = append(columns, c)
			}
		})
		rows = append(rows, row)
	}
	out := make([][]string, len(rows))
	for i, row := range rows {
		out[i] = make([]string, len(columns))
		for j, c := range columns {
			out[i][j] = row[c]
		}
	}
	return columns, out
}

var wrappers = map[string]bool{"data": true, "string_list_data": true, "string_map_data": true}

func flatten(v any, names []string, row map[string]string, column func(string)) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			next := names
			if !wrappers[k] && !(k == "value" && len(names) > 0) {
				next = append(append([]string{}, names...), k)
			}
			flatten(t[k], next, row, column)
		}
	case []any:
		scalars := []string{}
		for i, item := range t {
			switch item.(type) {
			case map[string]any, []any:
				next := names
				if i > 0 {
					next = append(append([]string{}, names...), fmt.Sprint(i+1))
				}
				flatten(item, next, row, column)
			default:
				scalars = append(scalars, str(item))
			}
		}
		if len(scalars) > 0 {
			set(names, strings.Join(scalars, "; "), row, column)
		}
	default:
		value := str(t)
		if n := len(names); n > 0 && strings.Contains(names[n-1], "timestamp") {
			if s, ok := number(t); ok && s > 0 {
				if strings.HasSuffix(names[n-1], "_ms") {
					s /= 1000
				}
				value = formatTime(unix(s))
			}
		}
		set(names, value, row, column)
	}
}

func set(names []string, value string, row map[string]string, column func(string)) {
	name := strings.Join(names, ".")
	if name == "" {
		name = "valor"
	}
	column(name)
	row[name] = value
}

// ---- Albums ----

func photoKey(v any, when time.Time) string {
	if media := mediaObjects(v); len(media) > 0 {
		return baseName(str(media[0]["uri"]))
	}
	return listKey(v, when)
}

func writeAlbum(ctx context.Context, run *plugin.Run, e export, r Rule, v any) error {
	m := asMap(v)
	name := str(m["name"])
	if name == "" {
		name = r.Title
	}
	folder := "albumes/" + plugin.Slug(name, 60)
	if plugin.Slug(name, 60) == "" {
		folder = "albumes/album-" + plugin.ShortHash(name)
	}
	photos := asList(m["photos"])
	if photos == nil {
		photos = Records(v)
	}
	var records []exportkit.Record
	for _, photo := range photos {
		when := TimeOf(photo)
		key := photoKey(photo, when)
		if err := copyMedia(ctx, run, e, photo, folder, when); err != nil {
			return err
		}
		records = append(records, record(photo, key, when))
	}
	list, added, err := exportkit.Merge(ctx, run.Out, folder+"/fotos.json", records, parser(photoKey), preferLocal)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}
	modTime := list[len(list)-1].Time
	// The album without its photos, which are in fotos.json.
	about := map[string]any{}
	for k, value := range m {
		if k != "photos" {
			about[k] = value
		}
	}
	about["name"] = name
	if _, err := run.Out.Write(ctx, plugin.File{Path: folder + "/album.json", ModTime: modTime, Data: plugin.JSON(about), Mutable: true}); err != nil {
		return err
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: folder + "/fotos.json", ModTime: modTime, Data: plugin.JSON(exportkit.Raws(list)), Mutable: true}); err != nil {
		return err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n", name)
	if d := str(m["description"]); d != "" {
		b.WriteString(exportkit.Quote(d) + "\n\n")
	}
	for _, r := range list {
		photo, _ := Decode(r.Raw)
		if !r.Time.IsZero() {
			fmt.Fprintf(&b, "## %s UTC\n\n", r.Time.Format("2006-01-02 15:04"))
		}
		if text, _ := textOf(photo); text != "" {
			b.WriteString(exportkit.Quote(text) + "\n\n")
		}
		if d := str(asMap(photo)["description"]); d != "" {
			b.WriteString(exportkit.Quote(d) + "\n\n")
		}
		mediaMarkdown(&b, photo, up(folder+"/album.md"))
	}
	if _, err := run.Out.Write(ctx, plugin.File{Path: folder + "/album.md", ModTime: modTime, Data: b.Bytes(), Mutable: true}); err != nil {
		return err
	}
	run.Item(added)
	return nil
}

// ---- Messages ----

func messageKey(v any, when time.Time) string {
	m := asMap(v)
	first := ""
	if media := mediaObjects(v); len(media) > 0 {
		first = baseName(str(media[0]["uri"]))
	}
	return fmt.Sprintf("%d-%s", when.UnixMilli(), plugin.ShortHash(str(m["sender_name"])+"\x00"+str(m["content"])+"\x00"+first))
}

func writeThread(ctx context.Context, run *plugin.Run, e export, thread string, files []any) error {
	folder := "mensajes/" + plugin.SafeName(thread, 80)
	title := ""
	var about map[string]any
	var records []exportkit.Record
	for _, file := range files {
		f := asMap(file)
		if about == nil {
			about = map[string]any{}
			for k, value := range f {
				if k != "messages" {
					about[k] = value
				}
			}
			title = str(f["title"])
		}
		for _, item := range asList(f["messages"]) {
			when := TimeOf(item)
			if when.IsZero() {
				continue
			}
			key := messageKey(item, when)
			if err := copyMedia(ctx, run, e, item, folder+"/media/"+when.Format("2006-01"), when); err != nil {
				return err
			}
			records = append(records, record(item, key, when))
		}
	}
	if title == "" {
		title = thread
	}
	if about != nil {
		if _, err := run.Out.Write(ctx, plugin.File{Path: folder + "/conversacion.json", ModTime: run.Now(), Data: plugin.JSON(about), Mutable: true}); err != nil {
			return err
		}
	}
	months, byMonth := exportkit.GroupByMonth(records)
	for _, month := range months {
		jsonPath := exportkit.MonthPath(folder, month)
		list, added, err := exportkit.Merge(ctx, run.Out, jsonPath, byMonth[month], parser(messageKey), preferLocal)
		if err != nil {
			return err
		}
		if err := exportkit.WriteMonth(ctx, run, jsonPath, list, messagesMarkdown(title, month, list, up(jsonPath))); err != nil {
			return err
		}
		run.Item(added)
	}
	return nil
}

func messagesMarkdown(title, month string, list []exportkit.Record, back string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s: %s\n\n", title, exportkit.MonthTitle(month))
	for _, r := range list {
		v, _ := Decode(r.Raw)
		m := asMap(v)
		fmt.Fprintf(&b, "**%s** · %s UTC\n\n", str(m["sender_name"]), r.Time.Format("2006-01-02 15:04"))
		if c := str(m["content"]); c != "" {
			b.WriteString(exportkit.Quote(c) + "\n\n")
		}
		mediaMarkdown(&b, v, back)
		for _, u := range links(v) {
			fmt.Fprintf(&b, "<%s>\n\n", u)
		}
		reactions := []string{}
		for _, item := range asList(m["reactions"]) {
			rm := asMap(item)
			reactions = append(reactions, strings.TrimSpace(str(rm["reaction"])+" "+str(rm["actor"])))
		}
		if len(reactions) > 0 {
			fmt.Fprintf(&b, "_Reacciones: %s_\n\n", strings.Join(reactions, ", "))
		}
	}
	return b.Bytes()
}
