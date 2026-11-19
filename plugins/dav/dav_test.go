package dav_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/dav"
	"github.com/fulldump/datame-suitcase/plugintest"
)

const (
	trabajo  = "/dav/calendars/ana/trabajo/"
	cumple   = "/dav/calendars/ana/cumplea%C3%B1os/"
	personal = "/dav/addressbooks/ana/personal/"
)

func fill(f *fakeDAV) {
	f.put(trabajo, "reunion.ics", meeting)
	f.put(trabajo, "nochevieja.ics", newYear("Nochevieja en Logroño 🎉"))
	f.put(trabajo, "viaje.ics", trip)
	f.put(cumple, "cumple-ana.ics", birthday)
	f.put(personal, "ana.vcf", ana)
	f.put(personal, "jose%20nunez.vcf", jose)
	f.put("/dav/addressbooks/ana/vacia/", "x.vcf", luis)
	f.remove("/dav/addressbooks/ana/vacia/", "x.vcf")
}

func config(server string) plugin.Config {
	return plugin.Config{"server": server, "user": "ana@example.org", "password": "clave-app", "calendars": "true", "contacts": "true"}
}

func has(t *testing.T, text, part string) {
	t.Helper()
	if !strings.Contains(text, part) {
		t.Fatalf("%q is not in:\n%s", part, text)
	}
}

func hasNot(t *testing.T, text, part string) {
	t.Helper()
	if strings.Contains(text, part) {
		t.Fatalf("%q should not be in:\n%s", part, text)
	}
}

func TestDAVSyncCollection(t *testing.T) {
	fake := newFake()
	fill(fake)
	server := httptest.NewServer(fake)
	defer server.Close()

	// Only the host: the calendars are found through /.well-known/.
	h := plugintest.New(t, dav.Plugin{}, config(server.URL))
	biff.AssertNil(h.Validate())

	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 6)

	// Each object as the server keeps it, line breaks included.
	biff.AssertEqual(string(h.File("Calendarios/Trabajo/eventos/reunion.ics")), meeting)
	biff.AssertEqual(string(h.File("Calendarios/Trabajo/eventos/viaje.ics")), trip)
	biff.AssertEqual(string(h.File("Calendarios/Cumpleanos/eventos/cumple-ana.ics")), birthday)
	biff.AssertEqual(string(h.File("Contactos/Personal/contactos/ana.vcf")), ana)
	biff.AssertEqual(string(h.File("Contactos/Personal/contactos/jose_nunez.vcf")), jose)

	// The merged calendar: every event, the shared time zone once, folded
	// lines intact.
	merged := string(h.File("Calendarios/Trabajo/Trabajo.ics"))
	biff.AssertEqual(strings.Count(merged, "BEGIN:VCALENDAR"), 1)
	biff.AssertEqual(strings.Count(merged, "BEGIN:VTIMEZONE"), 1)
	biff.AssertEqual(strings.Count(merged, "BEGIN:VEVENT"), 4)
	has(t, merged, "X-WR-CALNAME:Trabajo\r\n")
	has(t, merged, "porqu\r\n e pasa")
	hasNot(t, strings.ReplaceAll(merged, "\r\n", ""), "\n")

	summary := string(h.File("Calendarios/Trabajo/Trabajo.md"))
	has(t, summary, "# Trabajo")
	has(t, summary, "## 2025")
	has(t, summary, "### diciembre de 2025")
	has(t, summary, "- 2025-12-31, todo el día: **Nochevieja en Logroño 🎉** ([nochevieja.ics](eventos/nochevieja.ics))")
	has(t, summary, "- 2026-01-05 10:00 (Europe/Madrid): **Reunión de equipo** · Sala 2, planta 1 · se repite cada semana")
	has(t, summary, "### febrero de 2026")
	has(t, summary, "**Viaje a Zúrich** · Zürich HB")
	hasNot(t, summary, "movida") // a moved occurrence is part of its event
	has(t, string(h.File("Calendarios/Cumpleanos/Cumpleanos.md")), "1990-03-15, todo el día: **Cumpleaños de Ana** · se repite cada año")

	cards := string(h.File("Contactos/Personal/Personal.vcf"))
	biff.AssertEqual(strings.Count(cards, "BEGIN:VCARD"), 2)
	has(t, cards, photoLine1+"\r\n"+photoLine2+"\r\n")
	people := string(h.File("Contactos/Personal/Personal.md"))
	has(t, people, "- **Ana Pérez** · Tel.: +34 600 000 001 (móvil), +34 941 000 002 · Correo: ana@example.org (casa) · Cumpleaños: 1990-03-15 · con foto ([ana.vcf](contactos/ana.vcf))")
	has(t, people, "- **José Ñúñez** · Empresa: Ñandú S.L. · Tel.: +34-600-000-003 (trabajo) · Correo: jose@example.com (trabajo)")
	hasNot(t, people, "/9j/")
	biff.AssertTrue(strings.Index(people, "Ana") < strings.Index(people, "José"))
	has(t, string(h.File("Contactos/Personal/indice.json")), `"file": "ana.vcf"`)
	biff.AssertTrue(h.Has("Contactos/Vacia/Vacia.vcf"))

	// Nothing changed: the listing says so and nothing else is asked or written.
	puts, reports := h.NAS.Puts, fake.count("REPORT")
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)
	biff.AssertEqual(fake.count("REPORT"), reports)

	// An event changes, another is deleted, a contact is added.
	h.Now = h.Now.AddDate(0, 0, 5) // 2026-01-07
	fake.put(trabajo, "nochevieja.ics", newYear("Nochevieja en casa"))
	fake.remove(trabajo, "viaje.ics")
	fake.put(personal, "luis.vcf", luis)
	fake.hrefs = 0
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertEqual(fake.hrefs, 2) // only what changed was fetched
	biff.AssertTrue(fake.count("REPORT "+trabajo+` <?xml version="1.0" encoding="utf-8"?><D:sync-collection xmlns:D="DAV:"><D:sync-token>http://fake/sync/`) > 0)

	file, _ := h.NAS.File("principal", "/Archivo/Calendarios/Trabajo/eventos/nochevieja.ics")
	biff.AssertEqual(file.Versions, 2)
	biff.AssertEqual(string(h.File("Calendarios/Trabajo/eventos/viaje.ics")), trip) // a copy, not a mirror
	summary = string(h.File("Calendarios/Trabajo/Trabajo.md"))
	has(t, summary, "**Nochevieja en casa**")
	has(t, summary, "~~2026-02-10 08:30 (Europe/Madrid): **Viaje a Zúrich** · Zürich HB~~ · borrado en el servidor el 2026-01-07")
	has(t, summary, "Tiene 3 elementos, uno de ellos borrado en el servidor")
	merged = string(h.File("Calendarios/Trabajo/Trabajo.ics"))
	hasNot(t, merged, "Zúrich") // the merged file is the calendar as it is now
	has(t, merged, "Nochevieja en casa")
	has(t, string(h.File("Contactos/Personal/Personal.md")), "- **Luis Gómez** · Correo: luis@example.net")
	biff.AssertEqual(strings.Count(string(h.File("Contactos/Personal/Personal.vcf")), "BEGIN:VCARD"), 3)

	// The server forgets the tokens: everything is listed again, and only what
	// changed is fetched.
	fake.mutex.Lock()
	fake.minToken = fake.version + 1
	fake.mutex.Unlock()
	fake.put(trabajo, "reunion.ics", strings.Replace(meeting, "Sala 2", "Sala 3", 1))
	fake.hrefs = 0
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(fake.hrefs, 1)
	has(t, string(h.File("Calendarios/Trabajo/Trabajo.md")), "Sala 3")

	// Lost state and cache (a new install on the same folder): it goes on from
	// indice.json, fetches nothing, writes nothing new and keeps the deletion.
	h.Cursor, h.Manifest, h.CacheDir = nil, map[string]string{}, t.TempDir()
	created := h.NAS.Created
	fake.hrefs = 0
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(fake.hrefs, 0)
	biff.AssertEqual(h.NAS.Created, created)
	has(t, string(h.File("Calendarios/Trabajo/Trabajo.md")), "borrado en el servidor el 2026-01-07")
}

func TestDAVOnlyETags(t *testing.T) {
	fake := newFake()
	fake.noSync = true
	fill(fake)
	server := httptest.NewServer(fake)
	defer server.Close()

	// The address of the service itself, as for Nextcloud's /remote.php/dav.
	h := plugintest.New(t, dav.Plugin{}, config(server.URL+"/dav/"))
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 6)
	biff.AssertEqual(string(h.File("Calendarios/Trabajo/eventos/nochevieja.ics")), newYear("Nochevieja en Logroño 🎉"))

	puts := h.NAS.Puts
	fake.hrefs = 0
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(fake.hrefs, 0)
	biff.AssertEqual(h.NAS.Puts, puts)

	h.Now = h.Now.AddDate(0, 0, 1)
	fake.put(personal, "ana.vcf", strings.Replace(ana, "+34 600 000 001", "+34 699 999 999", 1))
	fake.remove(personal, "jose%20nunez.vcf")
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(fake.hrefs, 1)
	people := string(h.File("Contactos/Personal/Personal.md"))
	has(t, people, "+34 699 999 999")
	has(t, people, "~~**José Ñúñez**")
	has(t, people, "borrado en el servidor el 2026-01-03")
	biff.AssertTrue(h.Has("Contactos/Personal/contactos/jose_nunez.vcf"))
	biff.AssertEqual(fake.count("REPORT "+trabajo+` <?xml version="1.0" encoding="utf-8"?><D:sync-collection`), 0)
}

func TestDAVOnlyContacts(t *testing.T) {
	fake := newFake()
	fill(fake)
	server := httptest.NewServer(fake)
	defer server.Close()
	c := config(server.URL)
	c["calendars"] = "false"
	h := plugintest.New(t, dav.Plugin{}, c)
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertTrue(h.Has("Contactos/Personal/Personal.vcf"))
	biff.AssertFalse(h.Has("Calendarios/Trabajo/Trabajo.ics"))
}

func TestDAVWrongPassword(t *testing.T) {
	fake := newFake()
	fill(fake)
	server := httptest.NewServer(fake)
	defer server.Close()
	c := config(server.URL)
	c["password"] = "la-de-siempre"
	h := plugintest.New(t, dav.Plugin{}, c)

	err := h.Validate()
	biff.AssertNotNil(err)
	has(t, err.Error(), "no acepta el usuario o la contraseña")
	has(t, err.Error(), "contraseña de aplicación")
	r := h.Run()
	biff.AssertNotNil(r.Err)
	has(t, r.Err.Error(), "contraseña de aplicación")
	biff.AssertEqual(len(h.NAS.Paths("principal")), 0)
}

func TestDAVSettings(t *testing.T) {
	h := plugintest.New(t, dav.Plugin{}, config("ftp://x"))
	biff.AssertNotNil(h.Validate())
	h.Config = config("nextcloud")
	has(t, h.Validate().Error(), "remote.php/dav")
	h.Config = config("outlook")
	has(t, h.Validate().Error(), "DavMail")
	h.Config = config("https://ejemplo.org")
	h.Config["calendars"], h.Config["contacts"] = "false", "false"
	has(t, h.Validate().Error(), "al menos una")
}
