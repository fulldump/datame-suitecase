package takeout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

// Keep in Takeout: one JSON and one HTML per note, with the attached images
// and recordings beside them. All of it is copied to Keep/, and each note gets
// a Markdown with its title, text, list, labels and dates, which reads well
// anywhere.

type keepNote struct {
	Title       string `json:"title"`
	Text        string `json:"textContent"`
	ListContent []struct {
		Text      string `json:"text"`
		IsChecked bool   `json:"isChecked"`
	} `json:"listContent"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Created     int64 `json:"createdTimestampUsec"`
	Edited      int64 `json:"userEditedTimestampUsec"`
	Trashed     bool  `json:"isTrashed"`
	Pinned      bool  `json:"isPinned"`
	Archived    bool  `json:"isArchived"`
	Attachments []struct {
		FilePath string `json:"filePath"`
		Mimetype string `json:"mimetype"`
	} `json:"attachments"`
	Annotations []struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	} `json:"annotations"`
}

func (n keepNote) isNote() bool {
	return n.Created != 0 || n.Edited != 0 || n.Text != "" || len(n.ListContent) > 0
}

func usec(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(v).UTC()
}

func (x *importer) keep(ctx context.Context, e *entry, rel string) error {
	out := "Keep/" + cleanPath(rel)
	if !strings.EqualFold(path.Ext(rel), ".json") {
		_, err := x.raw(ctx, e, out, "")
		return err
	}
	result, err := x.raw(ctx, e, out, "notas de Keep")
	if err != nil || result == plugin.Unchanged {
		// The same note as before: its Markdown is there already, and making
		// it again could only differ by an attachment missing in this export.
		return err
	}
	data, err := e.readSmall(64 << 20)
	if err != nil {
		return err
	}
	var note keepNote
	if json.Unmarshal(data, &note) != nil || !note.isNote() {
		return nil
	}
	modTime := usec(note.Edited)
	if modTime.IsZero() {
		modTime = e.modTime
	}
	md := x.noteMarkdown(note, path.Dir(e.name))
	_, err = x.run.Out.Write(ctx, plugin.File{Path: strings.TrimSuffix(out, path.Ext(out)) + ".md", ModTime: modTime, Data: md, Mutable: true})
	return err
}

func (x *importer) noteMarkdown(n keepNote, dir string) []byte {
	var b bytes.Buffer
	title := n.Title
	if title == "" {
		title = "Nota sin título"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	facts := []string{}
	if t := usec(n.Created); !t.IsZero() {
		facts = append(facts, "Creada: "+t.Format("2006-01-02 15:04"))
	}
	if t := usec(n.Edited); !t.IsZero() {
		facts = append(facts, "Editada: "+t.Format("2006-01-02 15:04"))
	}
	if len(n.Labels) > 0 {
		names := []string{}
		for _, l := range n.Labels {
			names = append(names, l.Name)
		}
		facts = append(facts, "Etiquetas: "+strings.Join(names, ", "))
	}
	if n.Pinned {
		facts = append(facts, "Fijada")
	}
	if n.Archived {
		facts = append(facts, "Archivada")
	}
	if n.Trashed {
		facts = append(facts, "En la papelera")
	}
	if len(facts) > 0 {
		fmt.Fprintf(&b, "_%s_\n\n", strings.Join(facts, " · "))
	}
	if text := strings.TrimSpace(n.Text); text != "" {
		lines := strings.Split(text, "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ") + "  "
		}
		b.WriteString(strings.TrimRight(strings.Join(lines, "\n"), " "))
		b.WriteString("\n\n")
	}
	if len(n.ListContent) > 0 {
		for _, item := range n.ListContent {
			mark := " "
			if item.IsChecked {
				mark = "x"
			}
			fmt.Fprintf(&b, "- [%s] %s\n", mark, strings.ReplaceAll(item.Text, "\n", " "))
		}
		b.WriteString("\n")
	}
	for _, a := range n.Attachments {
		name := x.keepAttachment(dir, a.FilePath)
		link := markdownPath(cleanName(name))
		if strings.HasPrefix(a.Mimetype, "image/") {
			fmt.Fprintf(&b, "![%s](%s)\n\n", name, link)
		} else {
			fmt.Fprintf(&b, "[%s](%s)\n\n", name, link)
		}
	}
	for _, a := range n.Annotations {
		if a.URL != "" {
			label := a.Title
			if label == "" {
				label = a.URL
			}
			fmt.Fprintf(&b, "- [%s](%s)\n", label, a.URL)
		}
	}
	return b.Bytes()
}

// keepAttachment returns the file an attachment points to. Takeout sometimes
// names it «.jpeg» in the note and writes it as «.jpg», so a file with the
// same name and another extension is taken too.
func (x *importer) keepAttachment(dir, name string) string {
	files := x.idx.dirs[dir]
	if files[name] != nil {
		return name
	}
	stem := strings.TrimSuffix(name, path.Ext(name))
	for other := range files {
		if strings.TrimSuffix(other, path.Ext(other)) == stem && !strings.EqualFold(path.Ext(other), ".json") && !strings.EqualFold(path.Ext(other), ".html") {
			return other
		}
	}
	return name
}
