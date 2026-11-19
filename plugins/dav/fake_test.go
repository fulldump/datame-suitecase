package dav_test

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// fakeDAV is a small CalDAV and CardDAV server: well-known redirects,
// principal and home sets, PROPFIND, sync-collection with tokens, multiget and
// GET. With noSync it behaves as servers that only give ETags.
type fakeDAV struct {
	mutex    sync.Mutex
	noSync   bool
	minToken int // tokens below this are refused, as when a server forgets them
	version  int
	colls    map[string]*fakeColl // path → collection
	requests []string             // «METHOD path», and the body of REPORTs
	hrefs    int                  // hrefs asked for in multigets
}

type fakeColl struct {
	calendar bool
	name     string
	objects  map[string]*fakeObj // file name → object
	tombs    map[string]int      // file name → version when deleted
}

type fakeObj struct {
	data    string
	version int
}

const principal = "/dav/principals/ana/"

func newFake() *fakeDAV {
	return &fakeDAV{colls: map[string]*fakeColl{
		"/dav/calendars/ana/trabajo/":         {calendar: true, name: "Trabajo"},
		"/dav/calendars/ana/cumplea%C3%B1os/": {calendar: true, name: "Cumpleaños"},
		"/dav/addressbooks/ana/personal/":     {name: "Personal"},
		"/dav/addressbooks/ana/vacia/":        {name: "Vacía"},
	}}
}

// put creates or changes an object.
func (f *fakeDAV) put(coll, name, data string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	c := f.colls[coll]
	if c.objects == nil {
		c.objects, c.tombs = map[string]*fakeObj{}, map[string]int{}
	}
	f.version++
	c.objects[name] = &fakeObj{data: data, version: f.version}
	delete(c.tombs, name)
}

func (f *fakeDAV) remove(coll, name string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	c := f.colls[coll]
	f.version++
	delete(c.objects, name)
	c.tombs[name] = f.version
}

func (f *fakeDAV) count(prefix string) int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (c *fakeColl) version() int {
	v := 0
	for _, o := range c.objects {
		v = max(v, o.version)
	}
	for _, t := range c.tombs {
		v = max(v, t)
	}
	return v
}

var (
	tokenRe = regexp.MustCompile(`<D:sync-token>([^<]*)</D:sync-token>`)
	hrefRe  = regexp.MustCompile(`<D:href>([^<]*)</D:href>`)
)

// escape keeps the line breaks as they are, as real servers send them.
var escape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func (f *fakeDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := r.URL.EscapedPath()
	record := r.Method + " " + path
	if r.Method == "REPORT" {
		record += " " + string(body)
	}
	f.requests = append(f.requests, record)

	if strings.HasPrefix(path, "/.well-known/") {
		http.Redirect(w, r, "/dav/", http.StatusMovedPermanently)
		return
	}
	if user, password, ok := r.BasicAuth(); !ok || user != "ana@example.org" || password != "clave-app" {
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
		http.Error(w, "<html><body>Unauthorized</body></html>", http.StatusUnauthorized)
		return
	}

	var out strings.Builder
	resp := func(href, props string) {
		fmt.Fprintf(&out, `<D:response><D:href>%s</D:href><D:propstat><D:prop>%s</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>`+
			`<D:propstat><D:prop><D:missing-thing/></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response>`, href, props)
	}
	multistatus := func(extra string) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:CR="urn:ietf:params:xml:ns:carddav" xmlns:CS="http://calendarserver.org/ns/">%s%s</D:multistatus>`, out.String(), extra)
	}

	switch {
	case r.Method == "PROPFIND" && path == "/dav/":
		resp("/dav/", `<D:current-user-principal><D:href>`+principal+`</D:href></D:current-user-principal>`)
		multistatus("")
	case r.Method == "PROPFIND" && path == principal:
		// An absolute href, as iCloud gives.
		resp(principal, `<C:calendar-home-set><D:href>http://`+r.Host+`/dav/calendars/ana/</D:href></C:calendar-home-set>`+
			`<CR:addressbook-home-set><D:href>/dav/addressbooks/ana/</D:href></CR:addressbook-home-set>`)
		multistatus("")
	case r.Method == "PROPFIND" && (path == "/dav/calendars/ana/" || path == "/dav/addressbooks/ana/"):
		resp(path, `<D:resourcetype><D:collection/></D:resourcetype>`)
		var paths []string
		for p := range f.colls {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			c := f.colls[p]
			if !strings.HasPrefix(p, path) || c == nil {
				continue
			}
			kind := `<CR:addressbook/>`
			if c.calendar {
				kind = `<C:calendar/>`
			}
			props := `<D:resourcetype><D:collection/>` + kind + `</D:resourcetype><D:displayname>` + escape.Replace(c.name) + `</D:displayname>`
			if !f.noSync {
				props += fmt.Sprintf(`<D:sync-token>http://fake/sync/%d</D:sync-token><CS:getctag>%d</CS:getctag>`, c.version(), c.version()) +
					`<D:supported-report-set><D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report>` +
					`<D:supported-report><D:report><C:calendar-multiget/></D:report></D:supported-report></D:supported-report-set>`
			}
			// The display name of the birthdays comes with the non-ASCII path
			// written plainly, to check that both spellings are the same.
			href := p
			if strings.Contains(p, "%C3%B1") {
				href = strings.ReplaceAll(p, "%C3%B1", "ñ")
			}
			resp(href, props)
		}
		multistatus("")
	case r.Method == "PROPFIND" && f.colls[path] != nil:
		c := f.colls[path]
		resp(path, `<D:resourcetype><D:collection/></D:resourcetype>`)
		for _, name := range sortedNames(c) {
			resp(path+name, fmt.Sprintf(`<D:getetag>"%d"</D:getetag><D:resourcetype/>`, c.objects[name].version))
		}
		multistatus("")
	case r.Method == "REPORT" && f.colls[path] != nil && strings.Contains(string(body), "sync-collection"):
		if f.noSync {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		c := f.colls[path]
		since := 0
		if m := tokenRe.FindStringSubmatch(string(body)); m != nil && m[1] != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(m[1], "http://fake/sync/"))
			if err != nil || n < f.minToken {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `<?xml version="1.0"?><D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>`)
				return
			}
			since = n
		}
		for _, name := range sortedNames(c) {
			if o := c.objects[name]; o.version > since {
				resp(path+name, fmt.Sprintf(`<D:getetag>"%d"</D:getetag>`, o.version))
			}
		}
		for name, v := range c.tombs {
			if v > since && since > 0 {
				fmt.Fprintf(&out, `<D:response><D:href>%s</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`, path+name)
			}
		}
		multistatus(fmt.Sprintf(`<D:sync-token>http://fake/sync/%d</D:sync-token>`, c.version()))
	case r.Method == "REPORT" && f.colls[path] != nil && strings.Contains(string(body), "multiget"):
		c := f.colls[path]
		element := "CR:address-data"
		if c.calendar {
			element = "C:calendar-data"
		}
		for _, m := range hrefRe.FindAllStringSubmatch(string(body), -1) {
			f.hrefs++
			name := strings.TrimPrefix(m[1], path)
			o, ok := c.objects[name]
			if !ok {
				fmt.Fprintf(&out, `<D:response><D:href>%s</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`, m[1])
				continue
			}
			resp(m[1], fmt.Sprintf(`<D:getetag>"%d"</D:getetag><%s>%s</%s>`, o.version, element, escape.Replace(o.data), element))
		}
		multistatus("")
	case r.Method == "GET":
		dir := path[:strings.LastIndexByte(path, '/')+1]
		if c := f.colls[dir]; c != nil {
			if o, ok := c.objects[path[len(dir):]]; ok {
				w.Header().Set("ETag", fmt.Sprintf(`"%d"`, o.version))
				io.WriteString(w, o.data)
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func sortedNames(c *fakeColl) []string {
	var names []string
	for name := range c.objects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ---- Synthetic data ----

func crlf(lines ...string) string { return strings.Join(lines, "\r\n") + "\r\n" }

var madrid = []string{
	"BEGIN:VTIMEZONE", "TZID:Europe/Madrid",
	"BEGIN:STANDARD", "DTSTART:19701025T030000", "TZOFFSETFROM:+0200", "TZOFFSETTO:+0100", "RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU", "END:STANDARD",
	"BEGIN:DAYLIGHT", "DTSTART:19700329T020000", "TZOFFSETFROM:+0100", "TZOFFSETTO:+0200", "RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU", "END:DAYLIGHT",
	"END:VTIMEZONE",
}

func calendar(parts ...[]string) string {
	lines := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Fake//Server//EN"}
	for _, p := range parts {
		lines = append(lines, p...)
	}
	return crlf(append(lines, "END:VCALENDAR")...)
}

var meeting = calendar(madrid, []string{
	"BEGIN:VEVENT", "UID:reunion@fake", "DTSTAMP:20251201T000000Z",
	"DTSTART;TZID=Europe/Madrid:20260105T100000", "DTEND;TZID=Europe/Madrid:20260105T110000",
	"RRULE:FREQ=WEEKLY;BYDAY=MO", "SUMMARY:Reunión de equipo", "LOCATION:Sala 2\\, planta 1", "END:VEVENT",
	// A moved occurrence, in the same object.
	"BEGIN:VEVENT", "UID:reunion@fake", "DTSTAMP:20251201T000000Z", "RECURRENCE-ID;TZID=Europe/Madrid:20260112T100000",
	"DTSTART;TZID=Europe/Madrid:20260113T100000", "DTEND;TZID=Europe/Madrid:20260113T110000", "SUMMARY:Reunión de equipo (movida)", "END:VEVENT",
})

func newYear(title string) string {
	return calendar([]string{
		"BEGIN:VEVENT", "UID:nochevieja@fake", "DTSTAMP:20251201T000000Z",
		"DTSTART;VALUE=DATE:20251231", "DTEND;VALUE=DATE:20260101", "SUMMARY:" + title, "END:VEVENT",
	})
}

var trip = calendar(madrid, []string{
	"BEGIN:VEVENT", "UID:viaje@fake", "DTSTAMP:20251201T000000Z",
	"DTSTART;TZID=Europe/Madrid:20260210T083000", "DTEND;TZID=Europe/Madrid:20260212T200000",
	"SUMMARY:Viaje a Zúrich", "LOCATION:Zürich HB",
	"DESCRIPTION:Una descripción larga que el servidor dobla en varias líneas porqu",
	" e pasa de setenta y cinco octetos\\, como manda el estándar.",
	"END:VEVENT",
})

var birthday = calendar([]string{
	"BEGIN:VEVENT", "UID:cumple-ana@fake", "DTSTAMP:20251201T000000Z",
	"DTSTART;VALUE=DATE:19900315", "RRULE:FREQ=YEARLY", "SUMMARY:Cumpleaños de Ana", "END:VEVENT",
})

// photo is a fake JPEG, folded as the servers send it.
const photoLine1 = "PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAAMCAgMCAgMDAwMEAwMEBQgFBQQEBQoHBwYIDAoMDAsKCwsNDhIQ"
const photoLine2 = " DQ4RDgsLEBYQERMUFRUVDA8XGBYUGBIUFRT/2wBDAQMEBAUEBQkFBQkUDQsNFBQUFBQUFBQUFBQUFBQ=="

var ana = crlf("BEGIN:VCARD", "VERSION:3.0", "UID:ana@fake", "FN:Ana Pérez", "N:Pérez;Ana;;;",
	"TEL;TYPE=CELL,VOICE:+34 600 000 001", "item1.TEL:+34 941 000 002", "item1.X-ABLabel:Casa de campo",
	"EMAIL;TYPE=INTERNET,HOME:ana@example.org", "BDAY:1990-03-15", photoLine1, photoLine2, "END:VCARD")

var jose = crlf("BEGIN:VCARD", "VERSION:4.0", "UID:jose@fake", "FN:José Ñúñez", "N:Ñúñez;José;;;",
	"TEL;VALUE=uri;TYPE=work:tel:+34-600-000-003", "EMAIL;TYPE=work:jose@example.com", "ORG:Ñandú S.L.", "END:VCARD")

var luis = crlf("BEGIN:VCARD", "VERSION:3.0", "UID:luis@fake", "N:Gómez;Luis;;;", "EMAIL:luis@example.net", "END:VCARD")
