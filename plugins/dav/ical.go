package dav

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fulldump/datame-suitcase/plugin"
)

// iCalendar (RFC 5545) and vCard (RFC 6350) share one syntax: content lines
// «NAME;PARAM=value:value», folded at 75 bytes, inside BEGIN/END blocks. This
// is the little of it needed to merge files and write a summary. The objects
// themselves are always saved as the server gave them.

// line is one content line, unfolded.
type line struct {
	raw    string // as it was, still folded, without the last line break
	name   string // upper case, without the group of vCard («item1.TEL» → TEL)
	params map[string]string
	value  string
}

func parseLines(data []byte) []line {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var out []line
	var unfolded []string
	for _, physical := range strings.Split(text, "\n") {
		if (strings.HasPrefix(physical, " ") || strings.HasPrefix(physical, "\t")) && len(out) > 0 {
			out[len(out)-1].raw += "\r\n" + physical
			unfolded[len(unfolded)-1] += physical[1:]
			continue
		}
		if strings.TrimSpace(physical) == "" {
			continue
		}
		out = append(out, line{raw: physical})
		unfolded = append(unfolded, physical)
	}
	for i := range out {
		out[i].name, out[i].params, out[i].value = splitLine(unfolded[i])
	}
	return out
}

// splitLine separates name, parameters and value, minding quoted parameters
// that may hold «:» or «;».
func splitLine(s string) (string, map[string]string, string) {
	head, value := s, ""
	quoted := false
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			quoted = !quoted
		} else if s[i] == ':' && !quoted {
			head, value = s[:i], s[i+1:]
			break
		}
	}
	var parts []string
	quoted, start := false, 0
	for i := 0; i < len(head); i++ {
		if head[i] == '"' {
			quoted = !quoted
		} else if head[i] == ';' && !quoted {
			parts = append(parts, head[start:i])
			start = i + 1
		}
	}
	parts = append(parts, head[start:])
	if len(parts) == 0 {
		return "", nil, value
	}
	name := strings.ToUpper(strings.TrimSpace(parts[0]))
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	params := map[string]string{}
	for _, p := range parts[1:] {
		key, val, ok := strings.Cut(p, "=")
		if !ok {
			key, val = "TYPE", p // vCard 2.1: «TEL;CELL:…»
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		val = strings.Trim(val, `"`)
		if params[key] != "" {
			val = params[key] + "," + val
		}
		params[key] = val
	}
	return name, params, value
}

// component is a BEGIN/END block, with its own properties and the blocks inside.
type component struct {
	name     string
	props    []line
	children []*component
	lines    []line // every line from its BEGIN to its END, to copy it as it is
}

func (c *component) get(name string) (line, bool) {
	for _, l := range c.props {
		if l.name == name {
			return l, true
		}
	}
	return line{}, false
}

func (c *component) text(name string) string {
	l, _ := c.get(name)
	return unescape(l.value)
}

func (c *component) all(name string) []line {
	var out []line
	for _, l := range c.props {
		if l.name == name {
			out = append(out, l)
		}
	}
	return out
}

func parseComponents(data []byte) []*component {
	var top []*component
	var stack []*component
	for _, l := range parseLines(data) {
		switch l.name {
		case "BEGIN":
			c := &component{name: strings.ToUpper(strings.TrimSpace(l.value))}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, c)
			} else {
				top = append(top, c)
			}
			stack = append(stack, c)
		case "END":
			if len(stack) > 0 {
				for _, c := range stack {
					c.lines = append(c.lines, l)
				}
				stack = stack[:len(stack)-1]
			}
			continue
		default:
			if len(stack) > 0 {
				c := stack[len(stack)-1]
				c.props = append(c.props, l)
			}
		}
		for _, c := range stack {
			c.lines = append(c.lines, l)
		}
	}
	return top
}

func (c *component) raw() string {
	var b strings.Builder
	for _, l := range c.lines {
		b.WriteString(l.raw)
		b.WriteString("\r\n")
	}
	return b.String()
}

var textEscapes = strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`)

func unescape(s string) string { return textEscapes.Replace(s) }

func escapeText(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`).Replace(s)
}

// fold cuts a content line at 75 bytes without splitting a character.
func fold(s string) string {
	var b strings.Builder
	width := 0
	for _, r := range s {
		size := len(string(r))
		if width+size > 75 {
			b.WriteString("\r\n ")
			width = 1
		}
		b.WriteRune(r)
		width += size
	}
	return b.String() + "\r\n"
}

// stored is one object of a collection, read back for merging.
type stored struct {
	file    string // relative to the folder of the collection
	deleted string
	data    []byte
}

// ---- Merged files ----

// mergeCalendar joins the objects still on the server in one calendar that
// any application imports at once. Time zones shared by many events go once.
func mergeCalendar(name string, items []stored) []byte {
	var zones, others []string
	seen := map[string]bool{}
	for _, item := range items {
		if item.deleted != "" {
			continue
		}
		for _, top := range parseComponents(item.data) {
			if top.name != "VCALENDAR" {
				continue
			}
			for _, child := range top.children {
				if child.name == "VTIMEZONE" {
					id := child.text("TZID")
					if seen[id] {
						continue
					}
					seen[id] = true
					zones = append(zones, child.raw())
					continue
				}
				others = append(others, child.raw())
			}
		}
	}
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Datame//Suitcase//ES\r\nCALSCALE:GREGORIAN\r\n")
	b.WriteString(fold("X-WR-CALNAME:" + escapeText(name)))
	for _, z := range zones {
		b.WriteString(z)
	}
	for _, o := range others {
		b.WriteString(o)
	}
	b.WriteString("END:VCALENDAR\r\n")
	return []byte(b.String())
}

// mergeContacts joins the contacts still on the server in one .vcf, which is
// just the cards one after the other.
func mergeContacts(items []stored) []byte {
	var b strings.Builder
	for _, item := range items {
		if item.deleted != "" {
			continue
		}
		for _, top := range parseComponents(item.data) {
			if top.name == "VCARD" {
				b.WriteString(top.raw())
			}
		}
	}
	return []byte(b.String())
}

// ---- Summaries ----

var months = []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

var markdownSigns = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "[", `\[`, "]", `\]`, "<", `\<`, ">", `\>`, "#", `\#`)

// md makes a value safe in one line of Markdown.
func md(s string) string { return markdownSigns.Replace(strings.Join(strings.Fields(s), " ")) }

// when is a date or date-time as written in iCalendar, kept in its own time
// zone: the summary shows the time the person wrote, not converted.
type when struct {
	sortKey string // 20251231T100000, sorts as text
	year    int
	month   int
	text    string
	date    time.Time
	allDay  bool
}

func parseWhen(l line) (when, bool) {
	v := strings.TrimSpace(l.value)
	if len(v) < 8 {
		return when{}, false
	}
	date, err := time.Parse("20060102", v[:8])
	if err != nil {
		return when{}, false
	}
	w := when{sortKey: v, year: date.Year(), month: int(date.Month()), date: date, text: date.Format("2006-01-02")}
	if len(v) < 15 || strings.EqualFold(l.params["VALUE"], "DATE") {
		w.allDay = true
		w.sortKey = v[:8] + "T000000"
		return w, true
	}
	w.text += " " + v[9:11] + ":" + v[11:13]
	switch {
	case strings.HasSuffix(v, "Z"):
		w.text += " UTC"
	case l.params["TZID"] != "":
		w.text += " (" + l.params["TZID"] + ")"
	}
	return w, true
}

func repeats(rrule string) string {
	parts := map[string]string{}
	for _, p := range strings.Split(rrule, ";") {
		if k, v, ok := strings.Cut(p, "="); ok {
			parts[strings.ToUpper(k)] = strings.ToUpper(v)
		}
	}
	one := map[string]string{"DAILY": "cada día", "WEEKLY": "cada semana", "MONTHLY": "cada mes", "YEARLY": "cada año"}
	many := map[string]string{"DAILY": "días", "WEEKLY": "semanas", "MONTHLY": "meses", "YEARLY": "años"}
	text, ok := one[parts["FREQ"]]
	if !ok {
		return "se repite"
	}
	if n, _ := strconv.Atoi(parts["INTERVAL"]); n > 1 {
		text = fmt.Sprintf("cada %d %s", n, many[parts["FREQ"]])
	}
	return "se repite " + text
}

type entry struct {
	when    when
	dated   bool
	line    string
	sortKey string
}

func calendarSummary(col *collection, items []stored) []byte {
	var events, tasks []entry
	total, deleted := 0, 0
	for _, item := range items {
		counted := false
		for _, top := range parseComponents(item.data) {
			for _, c := range top.children {
				if c.name != "VEVENT" && c.name != "VTODO" {
					continue
				}
				// A changed occurrence of a repeating event lives in the same
				// file as the event; the summary lists the event once.
				if _, ok := c.get("RECURRENCE-ID"); ok {
					continue
				}
				if !counted {
					counted = true
					total++
					if item.deleted != "" {
						deleted++
					}
				}
				title := c.text("SUMMARY")
				if strings.TrimSpace(title) == "" {
					title = "(sin título)"
				}
				var e entry
				start, ok := c.get("DTSTART")
				if c.name == "VTODO" {
					if due, has := c.get("DUE"); has {
						start, ok = due, true
					}
				}
				if ok {
					e.when, e.dated = parseWhen(start)
				}
				var text strings.Builder
				if e.dated {
					text.WriteString(e.when.text)
					if e.when.allDay {
						if end, has := c.get("DTEND"); has {
							if w, good := parseWhen(end); good && w.date.Sub(e.when.date) > 24*time.Hour {
								text.WriteString(" al " + w.date.AddDate(0, 0, -1).Format("2006-01-02"))
							}
						}
						text.WriteString(", todo el día")
					}
					text.WriteString(": ")
				}
				text.WriteString("**" + md(title) + "**")
				if place := c.text("LOCATION"); strings.TrimSpace(place) != "" {
					text.WriteString(" · " + md(place))
				}
				if rule, has := c.get("RRULE"); has {
					text.WriteString(" · " + repeats(rule.value))
				}
				if strings.EqualFold(c.text("STATUS"), "CANCELLED") {
					text.WriteString(" · cancelado")
				}
				link := fmt.Sprintf(" ([%s](%s))", item.file[strings.LastIndexByte(item.file, '/')+1:], item.file)
				lineText := text.String()
				if c.name == "VTODO" {
					box := "[ ] "
					if strings.EqualFold(c.text("STATUS"), "COMPLETED") {
						box = "[x] "
					}
					lineText = box + lineText
				}
				if item.deleted != "" {
					lineText = "~~" + lineText + "~~ · borrado en el servidor el " + item.deleted
				}
				e.line = "- " + lineText + link
				e.sortKey = e.when.sortKey + " " + strings.ToLower(title)
				if c.name == "VTODO" {
					tasks = append(tasks, e)
				} else {
					events = append(events, e)
				}
			}
		}
	}
	sortEntries := func(list []entry) {
		sort.SliceStable(list, func(i, j int) bool { return list[i].sortKey < list[j].sortKey })
	}
	sortEntries(events)
	sortEntries(tasks)

	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n", md(col.Name))
	fmt.Fprintf(&b, "Calendario copiado de <%s>. %s%s", col.URL, plural(total, "Tiene un elemento", "Tiene %d elementos"), deletedNote(deleted))
	base := col.Folder[strings.LastIndexByte(col.Folder, '/')+1:]
	fmt.Fprintf(&b, ".\n\nCada uno está en su fichero en `eventos/`, y los que siguen en el servidor, todos juntos en `%s.ics`, que se importa de una vez en cualquier aplicación de calendario. Las horas son las del calendario, en su zona horaria.\n", base)
	year, month := 0, 0
	var undated []entry
	for _, e := range events {
		if !e.dated {
			undated = append(undated, e)
			continue
		}
		if e.when.year != year {
			year, month = e.when.year, 0
			fmt.Fprintf(&b, "\n## %d\n", year)
		}
		if e.when.month != month {
			month = e.when.month
			fmt.Fprintf(&b, "\n### %s de %d\n\n", months[month-1], year)
		}
		b.WriteString(e.line + "\n")
	}
	if len(undated) > 0 {
		b.WriteString("\n## Sin fecha\n\n")
		for _, e := range undated {
			b.WriteString(e.line + "\n")
		}
	}
	if len(tasks) > 0 {
		b.WriteString("\n## Tareas\n\n")
		for _, e := range tasks {
			b.WriteString(e.line + "\n")
		}
	}
	return b.Bytes()
}

var telTypes = map[string]string{"CELL": "móvil", "HOME": "casa", "WORK": "trabajo", "FAX": "fax", "MAIN": "principal", "IPHONE": "iPhone", "PAGER": "busca"}

func kinds(l line, names map[string]string) string {
	var out []string
	for _, t := range strings.Split(strings.ToUpper(l.params["TYPE"]), ",") {
		if name, ok := names[strings.TrimSpace(t)]; ok && !contains(out, name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return " (" + strings.Join(out, ", ") + ")"
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func contactName(c *component) string {
	if name := strings.TrimSpace(c.text("FN")); name != "" {
		return name
	}
	if n, ok := c.get("N"); ok {
		parts := strings.Split(n.value, ";")
		var words []string
		for _, i := range []int{3, 1, 2, 0, 4} { // prefix, given, middle, family, suffix
			if i < len(parts) && strings.TrimSpace(parts[i]) != "" {
				words = append(words, unescape(parts[i]))
			}
		}
		if len(words) > 0 {
			return strings.Join(words, " ")
		}
	}
	if org := strings.TrimSpace(strings.Split(c.text("ORG"), ";")[0]); org != "" {
		return org
	}
	if email := strings.TrimSpace(c.text("EMAIL")); email != "" {
		return email
	}
	return "(sin nombre)"
}

func contactsSummary(col *collection, items []stored) []byte {
	type person struct {
		key  string
		line string
	}
	var people []person
	deleted := 0
	for _, item := range items {
		for _, c := range parseComponents(item.data) {
			if c.name != "VCARD" {
				continue
			}
			name := contactName(c)
			var parts []string
			if strings.EqualFold(c.text("KIND"), "group") || strings.EqualFold(c.text("X-ADDRESSBOOKSERVER-KIND"), "group") {
				parts = append(parts, "grupo")
			}
			if org := strings.TrimSpace(strings.Split(c.text("ORG"), ";")[0]); org != "" && org != name {
				parts = append(parts, "Empresa: "+md(org))
			}
			var phones, emails []string
			for _, tel := range c.all("TEL") {
				number := strings.TrimSpace(strings.TrimPrefix(unescape(tel.value), "tel:"))
				if number != "" {
					phones = append(phones, md(number)+kinds(tel, telTypes))
				}
			}
			for _, email := range c.all("EMAIL") {
				if address := strings.TrimSpace(unescape(email.value)); address != "" {
					emails = append(emails, md(address)+kinds(email, map[string]string{"HOME": "casa", "WORK": "trabajo"}))
				}
			}
			if len(phones) > 0 {
				parts = append(parts, "Tel.: "+strings.Join(phones, ", "))
			}
			if len(emails) > 0 {
				parts = append(parts, "Correo: "+strings.Join(emails, ", "))
			}
			if bday := strings.TrimSpace(c.text("BDAY")); bday != "" {
				parts = append(parts, "Cumpleaños: "+md(bday))
			}
			if _, ok := c.get("PHOTO"); ok {
				parts = append(parts, "con foto")
			}
			text := "**" + md(name) + "**"
			if len(parts) > 0 {
				text += " · " + strings.Join(parts, " · ")
			}
			if item.deleted != "" {
				text = "~~" + text + "~~ · borrado en el servidor el " + item.deleted
			}
			text += fmt.Sprintf(" ([%s](%s))", item.file[strings.LastIndexByte(item.file, '/')+1:], item.file)
			people = append(people, person{key: plugin.Slug(name, 200) + " " + strings.ToLower(name), line: "- " + text})
		}
		if item.deleted != "" {
			deleted++
		}
	}
	sort.SliceStable(people, func(i, j int) bool { return people[i].key < people[j].key })

	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n", md(col.Name))
	fmt.Fprintf(&b, "Contactos copiados de <%s>. %s%s", col.URL, plural(len(people), "Hay un contacto", "Hay %d contactos"), deletedNote(deleted))
	base := col.Folder[strings.LastIndexByte(col.Folder, '/')+1:]
	fmt.Fprintf(&b, ".\n\nCada uno está en su fichero en `contactos/`, con su foto dentro si la tiene, y los que siguen en el servidor, todos juntos en `%s.vcf`, que se importa de una vez en cualquier aplicación de contactos.\n\n", base)
	for _, p := range people {
		b.WriteString(p.line + "\n")
	}
	return b.Bytes()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}

func deletedNote(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return ", uno de ellos borrado en el servidor (se conserva aquí, tachado)"
	}
	return fmt.Sprintf(", %d de ellos borrados en el servidor (se conservan aquí, tachados)", n)
}
