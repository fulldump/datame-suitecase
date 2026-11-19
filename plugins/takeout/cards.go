package takeout

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/fulldump/datame-suitcase/plugin"
)

// Calendars (.ics) and contacts (.vcf) are copied as they come: they are open
// standards that any calendar or address book opens. Beside each one goes a
// Markdown summary, to read them without any program.

// readProperties reads an iCalendar or vCard file as a stream, joining the
// folded lines (the ones that start with a space continue the one before),
// and calls fn with the name (upper case, without group), the parameters and
// the value of each line.
func readProperties(r io.Reader, fn func(name, params, value string)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	current := ""
	emit := func() {
		if current == "" {
			return
		}
		head, value, ok := strings.Cut(current, ":")
		if !ok {
			return
		}
		name, params, _ := strings.Cut(head, ";")
		if _, after, grouped := strings.Cut(name, "."); grouped {
			name = after // «item1.EMAIL»
		}
		fn(strings.ToUpper(name), strings.ToUpper(params), value)
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			current += line[1:]
			continue
		}
		emit()
		current = line
	}
	emit()
	return scanner.Err()
}

var textEscapes = strings.NewReplacer(`\n`, " ", `\N`, " ", `\,`, ",", `\;`, ";", `\\`, `\`)

func unescape(value string) string { return strings.TrimSpace(textEscapes.Replace(value)) }

// icsDate turns «20240105T100000Z» into «2024-01-05 10:00 UTC».
func icsDate(value string) string {
	v := strings.TrimSpace(value)
	if len(v) < 8 {
		return v
	}
	out := v[0:4] + "-" + v[4:6] + "-" + v[6:8]
	if len(v) >= 13 && v[8] == 'T' {
		out += " " + v[9:11] + ":" + v[11:13]
		if strings.HasSuffix(v, "Z") {
			out += " UTC"
		}
	}
	return out
}

type event struct {
	start, summary, location string
	repeats                  bool
}

func (x *importer) calendar(ctx context.Context, e *entry, out string) error {
	if _, err := x.raw(ctx, e, out, "calendarios"); err != nil {
		return err
	}
	r, err := e.open()
	if err != nil {
		return err
	}
	defer r.Close()
	name := strings.TrimSuffix(path.Base(out), path.Ext(out))
	events := []event{}
	var current *event
	err = readProperties(r, func(prop, params, value string) {
		switch {
		case prop == "X-WR-CALNAME" && current == nil:
			name = unescape(value)
		case prop == "BEGIN" && strings.EqualFold(value, "VEVENT"):
			current = &event{}
		case prop == "END" && strings.EqualFold(value, "VEVENT") && current != nil:
			events = append(events, *current)
			current = nil
		case current == nil:
		case prop == "DTSTART":
			current.start = value
		case prop == "SUMMARY":
			current.summary = unescape(value)
		case prop == "LOCATION":
			current.location = unescape(value)
		case prop == "RRULE":
			current.repeats = true
		}
	})
	if err != nil {
		return fmt.Errorf("%s: %w", e.name, err)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].start < events[j].start })

	var b bytes.Buffer
	fmt.Fprintf(&b, "# Calendario: %s\n\n", name)
	fmt.Fprintf(&b, "%d eventos. Todos los detalles están en `%s`, que se abre con cualquier programa de calendario.\n", len(events), path.Base(out))
	year := ""
	for _, ev := range events {
		when := icsDate(ev.start)
		if y := when[:min(4, len(when))]; y != year {
			year = y
			fmt.Fprintf(&b, "\n## %s\n\n", year)
		}
		title := ev.summary
		if title == "" {
			title = "(sin título)"
		}
		fmt.Fprintf(&b, "- %s · **%s**", when, title)
		if ev.location != "" {
			fmt.Fprintf(&b, " · %s", ev.location)
		}
		if ev.repeats {
			b.WriteString(" · se repite")
		}
		b.WriteString("\n")
	}
	_, err = x.run.Out.Write(ctx, plugin.File{Path: strings.TrimSuffix(out, path.Ext(out)) + ".md", ModTime: e.modTime, Data: b.Bytes(), Mutable: true})
	return err
}

type contact struct {
	name, org, birthday, note string
	emails, phones, addresses []string
}

func (x *importer) contacts(ctx context.Context, e *entry, out string) error {
	if _, err := x.raw(ctx, e, out, "libretas de contactos"); err != nil {
		return err
	}
	r, err := e.open()
	if err != nil {
		return err
	}
	defer r.Close()
	list := []contact{}
	var current *contact
	err = readProperties(r, func(prop, params, value string) {
		switch {
		case prop == "BEGIN" && strings.EqualFold(value, "VCARD"):
			current = &contact{}
		case prop == "END" && strings.EqualFold(value, "VCARD") && current != nil:
			list = append(list, *current)
			current = nil
		case current == nil:
		case prop == "FN":
			current.name = unescape(value)
		case prop == "N" && current.name == "":
			parts := strings.Split(value, ";")
			if len(parts) > 1 {
				current.name = unescape(strings.TrimSpace(parts[1] + " " + parts[0]))
			} else {
				current.name = unescape(value)
			}
		case prop == "EMAIL":
			current.emails = append(current.emails, unescape(value))
		case prop == "TEL":
			current.phones = append(current.phones, unescape(value))
		case prop == "ADR":
			fields := []string{}
			for _, f := range strings.Split(value, ";") {
				if f = unescape(f); f != "" {
					fields = append(fields, f)
				}
			}
			if len(fields) > 0 {
				current.addresses = append(current.addresses, strings.Join(fields, ", "))
			}
		case prop == "ORG":
			current.org = unescape(strings.Trim(strings.ReplaceAll(value, ";", ", "), ", "))
		case prop == "BDAY":
			current.birthday = unescape(value)
		case prop == "NOTE":
			current.note = unescape(value)
		}
	})
	if err != nil {
		return fmt.Errorf("%s: %w", e.name, err)
	}
	for i := range list {
		if list[i].name == "" {
			switch {
			case len(list[i].emails) > 0:
				list[i].name = list[i].emails[0]
			case len(list[i].phones) > 0:
				list[i].name = list[i].phones[0]
			default:
				list[i].name = "(sin nombre)"
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return strings.ToLower(list[i].name) < strings.ToLower(list[j].name) })

	var b bytes.Buffer
	title := strings.TrimSuffix(path.Base(out), path.Ext(out))
	fmt.Fprintf(&b, "# Contactos: %s\n\n", title)
	fmt.Fprintf(&b, "%d contactos. Todos los detalles están en `%s`, que se abre con cualquier libreta de direcciones.\n", len(list), path.Base(out))
	for _, c := range list {
		fmt.Fprintf(&b, "\n## %s\n\n", c.name)
		lines := []string{}
		for _, v := range c.emails {
			lines = append(lines, "Correo: "+v)
		}
		for _, v := range c.phones {
			lines = append(lines, "Teléfono: "+v)
		}
		for _, v := range c.addresses {
			lines = append(lines, "Dirección: "+v)
		}
		if c.org != "" {
			lines = append(lines, "Empresa: "+c.org)
		}
		if c.birthday != "" {
			lines = append(lines, "Cumpleaños: "+c.birthday)
		}
		if c.note != "" {
			lines = append(lines, "Nota: "+c.note)
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	_, err = x.run.Out.Write(ctx, plugin.File{Path: strings.TrimSuffix(out, path.Ext(out)) + ".md", ModTime: e.modTime, Data: b.Bytes(), Mutable: true})
	return err
}
