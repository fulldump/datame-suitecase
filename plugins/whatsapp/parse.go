package whatsapp

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// WhatsApp writes chats as text, one line per message, with a different
// header for each phone and each language:
//
//	Android: 19/11/26, 14:03 - Ana: hola
//	         11/19/26, 2:03 PM - Ana: hola
//	         19/11/2026, 2:03 p. m. - Ana: hola
//	iPhone:  [19/11/2026, 14:03:05] Ana: hola
//	         [11/19/26, 2:03:05 PM] Ana: hola
//
// A line without a header continues the message before. There are no ids and
// no time zones: the date is the wall clock the phone showed.

const (
	dateRe = `(\d{1,4}[./-]\d{1,2}[./-]\d{1,4})`
	timeRe = `(\d{1,2}[:.]\d{2}(?:[:.]\d{2})?)`
	ampmRe = `(?:\s*([AaPp]\.?\s?[Mm]\.?))?`
)

var (
	iphoneHeader  = regexp.MustCompile(`^\[` + dateRe + `,?\s+` + timeRe + ampmRe + `\]\s?(.*)$`)
	androidHeader = regexp.MustCompile(`^` + dateRe + `,?\s+` + timeRe + ampmRe + `\s[-–]\s(.*)$`)
	dateParts     = regexp.MustCompile(`\d+`)
)

// invisible are the direction marks WhatsApp sprinkles around names, dates
// and attachments; they make equal texts compare different.
var invisible = strings.NewReplacer(
	"\u200e", "", "\u200f", "", "\u202a", "", "\u202b", "", "\u202c", "", "\u202d", "", "\u202e", "",
	"\u2066", "", "\u2067", "", "\u2068", "", "\u2069", "", "\ufeff", "",
	"\u202f", " ", "\u00a0", " ", // narrow and non-breaking spaces before «p. m.»
)

// line is one message as read from the text, before deciding the date order.
type line struct {
	date, clock, ampm string
	iphone            bool
	author            string
	text              string
	system            bool
	marked            bool // the text started with a direction mark: iPhone does it for its own notices
}

// parseText reads a whole chat. It returns the raw lines, so that the date
// order (day/month or month/day) is decided looking at all of them.
func parseText(r io.Reader) ([]*line, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	var out []*line
	for scanner.Scan() {
		raw := scanner.Text()
		clean := invisible.Replace(raw)
		if l := header(clean); l != nil {
			// Was there a mark right before the text? iPhone marks its notices
			// («Los mensajes … están cifrados») and attachments that way.
			if _, after, ok := strings.Cut(raw, ": "); ok {
				l.marked = strings.HasPrefix(after, "\u200e")
			}
			out = append(out, l)
			continue
		}
		if len(out) > 0 {
			out[len(out)-1].text += "\n" + strings.TrimRight(clean, " ")
		}
	}
	return out, scanner.Err()
}

func header(s string) *line {
	l := &line{}
	var m []string
	if m = iphoneHeader.FindStringSubmatch(s); m != nil {
		l.iphone = true
	} else if m = androidHeader.FindStringSubmatch(s); m == nil {
		return nil
	}
	l.date, l.clock, l.ampm = m[1], m[2], m[3]
	rest := m[4]
	if author, text, ok := strings.Cut(rest, ": "); ok && plausibleAuthor(author) {
		l.author, l.text = strings.TrimSpace(author), text
	} else if author, ok := strings.CutSuffix(rest, ":"); ok && plausibleAuthor(author) {
		l.author = strings.TrimSpace(author) // a message with an empty first line
	} else {
		l.system, l.text = true, rest
	}
	return l
}

// plausibleAuthor keeps notices like «Ana cambió el asunto a "Viaje: Roma"»
// from being read as a message of «Ana cambió el asunto a "Viaje».
func plausibleAuthor(s string) bool {
	return s != "" && len(s) <= 80 && !strings.ContainsAny(s, "\"“”«»")
}

// dateOrder decides whether ambiguous dates are day/month or month/day. A
// part over 12 settles it; otherwise English 12-hour clocks are taken as the
// American month/day, and everything else as day/month.
func dateOrder(lines []*line) string {
	english := false
	for _, l := range lines {
		p := dateParts.FindAllString(l.date, -1)
		if len(p) != 3 || len(p[0]) == 4 {
			continue
		}
		a, _ := strconv.Atoi(p[0])
		b, _ := strconv.Atoi(p[1])
		if a > 12 {
			return "dmy"
		}
		if b > 12 {
			return "mdy"
		}
		if l.ampm != "" && !strings.Contains(l.ampm, ".") {
			english = true
		}
	}
	if english {
		return "mdy"
	}
	return "dmy"
}

func (l *line) when(order string) (time.Time, error) {
	p := dateParts.FindAllString(l.date, -1)
	if len(p) != 3 {
		return time.Time{}, fmt.Errorf("fecha no válida: %s", l.date)
	}
	n := make([]int, 3)
	for i := range p {
		n[i], _ = strconv.Atoi(p[i])
	}
	var year, month, day int
	switch {
	case len(p[0]) == 4:
		year, month, day = n[0], n[1], n[2]
	case order == "mdy":
		month, day, year = n[0], n[1], n[2]
	default:
		day, month, year = n[0], n[1], n[2]
	}
	if year < 100 {
		year += 2000
	}
	c := dateParts.FindAllString(l.clock, -1)
	hour, _ := strconv.Atoi(c[0])
	minute, _ := strconv.Atoi(c[1])
	second := 0
	if len(c) == 3 {
		second, _ = strconv.Atoi(c[2])
	}
	if l.ampm != "" {
		pm := unicode.ToLower(rune(l.ampm[0])) == 'p'
		hour %= 12
		if pm {
			hour += 12
		}
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
	if t.Month() != time.Month(month) || t.Day() != day || hour > 23 || minute > 59 {
		return time.Time{}, fmt.Errorf("fecha no válida: %s %s", l.date, l.clock)
	}
	return t, nil
}

// ---- Attachments ----

var (
	// iPhone: «<attached: 00000012-PHOTO-2026-11-19-14-03-05.jpg>», «<adjunto: …>».
	iphoneAttached = regexp.MustCompile(`<([^<>:]{1,30}): ([^<>]+)>`)
	// Android: «IMG-20261119-WA0001.jpg (archivo adjunto)».
	androidAttached = regexp.MustCompile(`^(.+?) \(([^()]+)\)$`)
	// Android without media: «<Multimedia omitido>», «<Media omitted>».
	androidOmitted = regexp.MustCompile(`(?i)^<[^<>]*(omitid|omitted|omis|ausgeschlossen|weggelassen)[^<>]*>$`)
	// iPhone without media: «image omitted», «imagen omitida», «Contact card omitted».
	iphoneOmitted = regexp.MustCompile(`(?i)^[\p{L}\p{N} ]{1,40} (omitted|omitida|omitido|omitidos|omitidas|omise|omesso)$`)
	// iPhone numbers files in each export: «00000012-». The rest is stable.
	iphoneNumber = regexp.MustCompile(`^\d{8}-`)
	// Names that already tell one file from another: WhatsApp's own.
	selfNamed = regexp.MustCompile(`(?i)(-WA\d{4,}|^(PHOTO|VIDEO|AUDIO|GIF|STICKER)-\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2})`)
)

var attachedLabels = map[string]bool{
	"attached": true, "adjunto": true, "archivo adjunto": true, "anexado": true, "allegato": true,
	"angehängt": true, "pièce jointe": true, "bijlage": true, "archivo": true,
	"file attached": true, "fichero adjunto": true, "arquivo anexado": true, "file allegato": true,
	"datei angehängt": true, "fichier joint": true, "bestand bijgevoegd": true,
}

// attachment finds a file in the text of a message. It returns the name as
// written, the text without the marker (the caption), and whether media was
// there but not exported.
func attachment(text string, exists func(string) bool) (name, caption string, omitted bool) {
	first, rest, _ := strings.Cut(text, "\n")
	if trimmed := strings.TrimSpace(first); androidOmitted.MatchString(trimmed) || iphoneOmitted.MatchString(trimmed) {
		return "", strings.TrimSpace(rest), true
	}
	if m := iphoneAttached.FindStringSubmatchIndex(text); m != nil {
		label := strings.ToLower(strings.TrimSpace(text[m[2]:m[3]]))
		file := strings.TrimSpace(text[m[4]:m[5]])
		if attachedLabels[label] || exists(file) {
			caption := text[:m[0]] + text[m[1]:]
			// iPhone writes «Factura.pdf • 3 páginas <attached: …>»: that
			// prefix only repeats the name.
			if before, _, ok := strings.Cut(strings.TrimSpace(text[:m[0]]), " • "); ok && strings.HasSuffix(file, before) {
				caption = text[m[1]:]
			}
			return file, strings.TrimSpace(caption), false
		}
	}
	if m := androidAttached.FindStringSubmatch(strings.TrimSpace(first)); m != nil {
		file := m[1]
		if before, _, ok := strings.Cut(file, " • "); ok { // «Factura.pdf • 3 páginas»
			file = before
		}
		if attachedLabels[strings.ToLower(m[2])] || exists(file) {
			return file, strings.TrimSpace(rest), false
		}
	}
	return "", text, false
}

// stableName is the name a media file keeps across exports: iPhone's
// per-export number is dropped, and names that do not tell files apart
// («Factura.pdf») get the time of the message in front.
func stableName(written string, when time.Time) string {
	name := iphoneNumber.ReplaceAllString(path.Base(strings.ReplaceAll(written, "\\", "/")), "")
	if !selfNamed.MatchString(name) {
		name = when.Format("20060102-150405") + "-" + name
	}
	return name
}
