package takeout_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/takeout"
	"github.com/fulldump/datame-suitcase/plugintest"
)

type member struct{ name, content string }

var zipTime = time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)

// writeZip makes a synthetic Takeout part.
func writeZip(t *testing.T, file string, members []member) {
	t.Helper()
	f, err := os.Create(file)
	biff.AssertNil(err)
	z := zip.NewWriter(f)
	for _, m := range members {
		w, err := z.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Deflate, Modified: zipTime})
		biff.AssertNil(err)
		w.Write([]byte(m.content))
	}
	biff.AssertNil(z.Close())
	biff.AssertNil(f.Close())
}

func writeTgz(t *testing.T, file string, members []member) {
	t.Helper()
	f, err := os.Create(file)
	biff.AssertNil(err)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		biff.AssertNil(tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.content)), ModTime: zipTime, Typeflag: tar.TypeReg}))
		tw.Write([]byte(m.content))
	}
	biff.AssertNil(tw.Close())
	biff.AssertNil(gz.Close())
	biff.AssertNil(f.Close())
}

func sidecar(title string, taken time.Time) string {
	return fmt.Sprintf(`{"title":%q,"description":"","photoTakenTime":{"timestamp":"%d","formatted":"x"},"geoData":{"latitude":40.4,"longitude":-3.7}}`, title, taken.Unix())
}

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 10, 0, 0, 0, time.UTC) }

func message(id, labels, date, subject string) string {
	return "From 1790000000000000001@xxx Fri Mar 01 10:00:00 +0000 2024\r\n" +
		"X-GM-THRID: 1790000000000000001\r\n" +
		"X-Gmail-Labels: " + labels + "\r\n" +
		"Message-ID: <" + id + "@example.org>\r\n" +
		"Date: " + date + "\r\n" +
		"From: Ana <ana@example.org>\r\n" +
		"To: yo@example.org\r\n" +
		"Subject: " + subject + "\r\n" +
		"\r\n" +
		"Hola, ¿qué tal?\r\n" +
		">From the text, escaped by Gmail\r\n" +
		"\r\n"
}

var (
	msgOne   = message("uno", "Recibidos,Importante,Abierto", "Fri, 01 Mar 2024 10:00:00 +0000", "Hola")
	msgTwo   = message("dos", `Recibidos,"Viajes, 2024"`, "Mon, 15 Apr 2024 18:30:00 +0200", "Billetes")
	msgThree = message("tres", "Enviados", "Tue, 10 Dec 2024 09:00:00 +0000", "Nuevo")
)

const keepNote = `{"color":"DEFAULT","isTrashed":false,"isPinned":true,"isArchived":false,
"title":"Lista de la compra","textContent":"","listContent":[{"text":"Pan","isChecked":true},{"text":"Leche","isChecked":false}],
"labels":[{"name":"Casa"}],"attachments":[{"filePath":"1a2b.jpeg","mimetype":"image/jpeg"}],
"createdTimestampUsec":1704103200000000,"userEditedTimestampUsec":1704189600000000}`

const calendar = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nX-WR-CALNAME:Personal\r\n" +
	"BEGIN:VEVENT\r\nDTSTART:20240105T100000Z\r\nSUMMARY:Cena con Ana\r\nLOCATION:Casa de Ana\\, Madrid\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nDTSTART;VALUE=DATE:20230301\r\nSUMMARY:Cumpleaños de\r\n  Luis\r\nRRULE:FREQ=YEARLY\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

const contacts = "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Ana Pérez\r\nN:Pérez;Ana;;;\r\nitem1.EMAIL;TYPE=INTERNET:ana@example.org\r\nTEL;TYPE=CELL:+34 600 000 000\r\nEND:VCARD\r\n" +
	"BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Bruno\r\nORG:Ejemplo S.L.\r\nEND:VCARD\r\n"

const records = `{"locations":[
{"latitudeE7":404167754,"longitudeE7":-37037902,"accuracy":20,"timestamp":"2024-01-01T10:00:00.123Z"},
{"latitudeE7":413850000,"longitudeE7":21730000,"timestampMs":"1577872800000"}]}`

// spanishExport is an export in two parts, with the folder names of a
// Spanish account. The JSON of IMG_0003 is in the first part and the photo in
// the second.
func spanishExport(t *testing.T, dir string) {
	photos := "Takeout/Google Fotos/"
	writeZip(t, filepath.Join(dir, "takeout-20250101T100000Z-001.zip"), []member{
		{"Takeout/archive_browser.html", "<html>índice</html>"},
		{photos + "Fotos de 2024/IMG_0001.jpg", "foto-uno"},
		{photos + "Fotos de 2024/IMG_0001.jpg.supplemental-metadata.json", sidecar("IMG_0001.jpg", day(2024, 5, 6))},
		{photos + "Vacaciones en la playa/IMG_0001.jpg", "foto-uno"},
		{photos + "Vacaciones en la playa/IMG_0001.jpg.json", sidecar("IMG_0001.jpg", day(2024, 5, 6))},
		{photos + "Vacaciones en la playa/metadatos.json", `{"title":"Vacaciones en la playa","description":"Verano en Cádiz"}`},
		{photos + "Vacaciones en la playa/IMG_0003.jpg.supplemental-metadata.json", sidecar("IMG_0003.jpg", day(2024, 8, 15))},
		{photos + "Fotos de 2024/PXL_20240607_123456789.PORTRAIT.ORIGINAL.jpg", "foto-larga"},
		{photos + "Fotos de 2024/PXL_20240607_123456789.PORTRAIT.ORIGINAL.jpg.supplemental-met.json", sidecar("PXL_20240607_123456789.PORTRAIT.ORIGINAL.jpg", day(2024, 6, 7))},
		{photos + "Fotos de 2024/IMG_0002.jpg", "foto-dos-a"},
		{photos + "Fotos de 2024/IMG_0002.jpg.supplemental-metadata.json", sidecar("IMG_0002.jpg", day(2024, 7, 1))},
		{photos + "Fotos de 2024/IMG_0002(1).jpg", "foto-dos-b"},
		{photos + "Fotos de 2024/IMG_0002.jpg.supplemental-metadata(1).json", sidecar("IMG_0002.jpg", day(2024, 8, 1))},
		{photos + "Fotos de 2024/huerfano.json", `{"suelto":true}`},
		{"Takeout/Correo/Todo el correo, incluido Spam y Papelera.mbox", msgOne + msgTwo},
	})
	writeZip(t, filepath.Join(dir, "takeout-20250101T100000Z-002.zip"), []member{
		{photos + "Vacaciones en la playa/IMG_0003.jpg", "foto-tres"},
		{"Takeout/Keep/Lista de la compra.json", keepNote},
		{"Takeout/Keep/Lista de la compra.html", "<html>nota</html>"},
		{"Takeout/Keep/1a2b.jpg", "adjunto"},
		{"Takeout/Calendario/Personal.ics", calendar},
		{"Takeout/Contactos/Todos los contactos/Todos los contactos.vcf", contacts},
		{"Takeout/Google Pay/Transacciones.csv", "fecha,importe\n2024-01-01,3\n"},
		{"Takeout/Drive/Mis cosas/notas.txt", "mis notas"},
		{"Takeout/Historial de ubicaciones/Records.json", records},
	})
}

// countContent counts the files of the NAS with this content.
func countContent(h *plugintest.Harness, content string) int {
	n := 0
	for _, p := range h.NAS.Paths("principal") {
		if f, _ := h.NAS.File("principal", p); string(f.Data) == content {
			n++
		}
	}
	return n
}

func pathsWith(h *plugintest.Harness, prefix, suffix string) []string {
	out := []string{}
	for _, p := range h.NAS.Paths("principal") {
		if strings.HasPrefix(p, "/Archivo/"+prefix) && strings.HasSuffix(p, suffix) {
			out = append(out, strings.TrimPrefix(p, "/Archivo/"))
		}
	}
	return out
}

func TestTakeoutImport(t *testing.T) {
	drop := t.TempDir()
	spanishExport(t, drop)

	h := plugintest.New(t, takeout.Plugin{}, plugin.Config{"path": drop})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	// 5 photos, 2 messages, 1 note, 1 calendar, 1 address book, Drive, Google Pay and the index.
	biff.AssertEqual(r.Items, 13)

	// Photos: once each, by date, with the date of the JSON and the JSON beside.
	biff.AssertEqual(string(h.File("Fotos/2024/2024-05/IMG_0001.jpg")), "foto-uno")
	biff.AssertEqual(countContent(h, "foto-uno"), 1)
	file, _ := h.NAS.File("principal", "/Archivo/Fotos/2024/2024-05/IMG_0001.jpg")
	biff.AssertEqual(file.ModTime, day(2024, 5, 6).Unix())
	biff.AssertTrue(strings.Contains(string(h.File("Fotos/2024/2024-05/IMG_0001.jpg.json")), "photoTakenTime"))
	biff.AssertEqual(string(h.File("Fotos/2024/2024-06/PXL_20240607_123456789.PORTRAIT.ORIGINAL.jpg")), "foto-larga")
	biff.AssertTrue(h.Has("Fotos/2024/2024-06/PXL_20240607_123456789.PORTRAIT.ORIGINAL.jpg.json"))
	biff.AssertEqual(string(h.File("Fotos/2024/2024-07/IMG_0002.jpg")), "foto-dos-a")
	biff.AssertEqual(string(h.File("Fotos/2024/2024-08/IMG_0002(1).jpg")), "foto-dos-b")
	biff.AssertEqual(string(h.File("Fotos/2024/2024-08/IMG_0003.jpg")), "foto-tres") // its JSON was in the other part
	biff.AssertTrue(h.Has("Fotos/otros/Fotos de 2024/huerfano.json"))
	album := string(h.File("Fotos/Albumes/Vacaciones en la playa/album.md"))
	biff.AssertTrue(strings.Contains(album, "# Álbum: Vacaciones en la playa"))
	biff.AssertTrue(strings.Contains(album, "Verano en Cádiz"))
	biff.AssertTrue(strings.Contains(album, "(../../2024/2024-05/IMG_0001.jpg)"))
	biff.AssertTrue(strings.Contains(album, "(../../2024/2024-08/IMG_0003.jpg)"))
	biff.AssertTrue(h.Has("Fotos/Albumes/Vacaciones en la playa/metadata.json"))

	// Mail: the mbox as it came, and one .eml per message in the folder of its label.
	biff.AssertEqual(string(h.File("Correo/mbox/Todo el correo, incluido Spam y Papelera.mbox")), msgOne+msgTwo)
	inbox := pathsWith(h, "Correo/Recibidos/2024/2024-03/2024-03-01_100000-", ".eml")
	biff.AssertEqual(len(inbox), 1)
	eml := string(h.File(inbox[0]))
	biff.AssertTrue(strings.HasPrefix(eml, "X-GM-THRID:"))
	biff.AssertTrue(strings.Contains(eml, "Subject: Hola"))
	biff.AssertEqual(len(pathsWith(h, "Correo/Viajes, 2024/2024/2024-04/2024-04-15_163000-", ".eml")), 1)

	// Keep: the note as it came, in Markdown, and its attachment.
	note := string(h.File("Keep/Lista de la compra.md"))
	biff.AssertTrue(strings.Contains(note, "# Lista de la compra"))
	biff.AssertTrue(strings.Contains(note, "- [x] Pan\n- [ ] Leche"))
	biff.AssertTrue(strings.Contains(note, "Etiquetas: Casa · Fijada"))
	biff.AssertTrue(strings.Contains(note, "![1a2b.jpg](1a2b.jpg)"))
	biff.AssertEqual(string(h.File("Keep/1a2b.jpg")), "adjunto")
	biff.AssertTrue(h.Has("Keep/Lista de la compra.json"))
	biff.AssertTrue(h.Has("Keep/Lista de la compra.html"))

	// Calendar and contacts, with their summaries.
	biff.AssertEqual(string(h.File("Calendario/Personal.ics")), calendar)
	cal := string(h.File("Calendario/Personal.md"))
	biff.AssertTrue(strings.Contains(cal, "## 2024\n\n- 2024-01-05 10:00 UTC · **Cena con Ana** · Casa de Ana, Madrid"))
	biff.AssertTrue(strings.Contains(cal, "- 2023-03-01 · **Cumpleaños de Luis** · se repite"))
	people := string(h.File("Contactos/Todos los contactos/Todos los contactos.md"))
	biff.AssertTrue(strings.Contains(people, "## Ana Pérez\n\n- Correo: ana@example.org\n- Teléfono: +34 600 000 000"))
	biff.AssertTrue(strings.Contains(people, "## Bruno\n\n- Empresa: Ejemplo S.L."))

	// Location history: as it came, and a CSV per year.
	biff.AssertTrue(h.Has("Ubicaciones/Records.json"))
	biff.AssertEqual(string(h.File("Ubicaciones/resumen/2024.csv")), "fecha_utc,latitud,longitud,precision_m\n2024-01-01T10:00:00Z,40.4167754,-3.7037902,20\n")
	biff.AssertTrue(h.Has("Ubicaciones/resumen/2020.csv"))

	// Everything else, as it came.
	biff.AssertEqual(string(h.File("Drive/Mis cosas/notas.txt")), "mis notas")
	biff.AssertTrue(h.Has("Otros/Google Pay/Transacciones.csv"))
	biff.AssertTrue(h.Has("Otros/Takeout/archive_browser.html"))
	biff.AssertFalse(h.Has("Originales/takeout-20250101T100000Z-001.zip")) // off by default

	// The same folder again: nothing is read or written.
	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A newer export, of the same account now in English: IMG_0001 again, a
	// new photo also added to the album, message one archived (other label),
	// and a new message.
	writeZip(t, filepath.Join(drop, "takeout-20250301T100000Z-001.zip"), []member{
		{"Takeout/Google Photos/Photos from 2024/IMG_0001.jpg", "foto-uno"},
		{"Takeout/Google Photos/Photos from 2024/IMG_0001.jpg.supplemental-metadata.json", sidecar("IMG_0001.jpg", day(2024, 5, 6))},
		{"Takeout/Google Photos/Photos from 2024/IMG_0004.jpg", "foto-cuatro"},
		{"Takeout/Google Photos/Photos from 2024/IMG_0004.jpg.supplemental-metadata.json", sidecar("IMG_0004.jpg", day(2024, 12, 24))},
		{"Takeout/Google Photos/Vacaciones en la playa/IMG_0004.jpg", "foto-cuatro"},
		{"Takeout/Google Photos/Vacaciones en la playa/IMG_0004.jpg.supplemental-metadata.json", sidecar("IMG_0004.jpg", day(2024, 12, 24))},
		{"Takeout/Google Photos/Vacaciones en la playa/metadata.json", `{"title":"Vacaciones en la playa","description":"Verano en Cádiz"}`},
		{"Takeout/Mail/All mail Including Spam and Trash.mbox", strings.Replace(msgOne, "Recibidos,Importante,Abierto", "Archived", 1) + msgTwo + msgThree},
		{"Takeout/Keep/Lista de la compra.json", keepNote},
		{"Takeout/Calendar/Personal.ics", calendar},
	})
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2) // IMG_0004 and message three
	biff.AssertEqual(countContent(h, "foto-uno"), 1)
	biff.AssertEqual(countContent(h, "foto-cuatro"), 1)
	biff.AssertEqual(len(pathsWith(h, "Correo/", ".eml")), 3)
	biff.AssertEqual(len(pathsWith(h, "Correo/Archived/", "")), 0)
	biff.AssertEqual(len(pathsWith(h, "Correo/Enviados/2024/2024-12/", ".eml")), 1)
	album = string(h.File("Fotos/Albumes/Vacaciones en la playa/album.md"))
	for _, p := range []string{"2024-05/IMG_0001.jpg", "2024-08/IMG_0003.jpg", "2024-12/IMG_0004.jpg"} {
		biff.AssertTrue(strings.Contains(album, p))
	}
	note1, _ := h.NAS.File("principal", "/Archivo/Keep/Lista de la compra.md")
	biff.AssertEqual(note1.Versions, 1) // an unchanged note is not touched

	// A new install over the same NAS (cursor and list of files lost) writes
	// nothing new.
	h.Cursor, h.Manifest = nil, map[string]string{}
	created, replaced := h.NAS.Created, h.NAS.Replace
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Created, created)
	biff.AssertEqual(h.NAS.Replace, replaced)
}

func TestTakeoutWaitsForMissingParts(t *testing.T) {
	drop := t.TempDir()
	writeZip(t, filepath.Join(drop, "takeout-20250101T100000Z-001.zip"), []member{{"Takeout/Drive/a.txt", "a"}})
	writeZip(t, filepath.Join(drop, "takeout-20250101T100000Z-003.zip"), []member{{"Takeout/Drive/c.txt", "c"}})
	// A part still being downloaded is not a valid ZIP yet.
	biff.AssertNil(os.WriteFile(filepath.Join(drop, "takeout-20250201T100000Z-001.zip"), []byte("PK a medias"), 0o644))

	h := plugintest.New(t, takeout.Plugin{}, plugin.Config{"path": drop})
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertFalse(h.Has("Drive/a.txt"))

	writeZip(t, filepath.Join(drop, "takeout-20250101T100000Z-002.zip"), []member{{"Takeout/Drive/b.txt", "b"}})
	writeZip(t, filepath.Join(drop, "takeout-20250201T100000Z-001.zip"), []member{{"Takeout/Drive/d.txt", "d"}})
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 4)
	biff.AssertTrue(h.Has("Drive/c.txt"))
	biff.AssertTrue(h.Has("Drive/d.txt"))
}

func TestTakeoutTgzAndChoices(t *testing.T) {
	drop := t.TempDir()
	// In a .tgz the JSON of a photo may come after the photo.
	writeTgz(t, filepath.Join(drop, "takeout-20250101T100000Z-001.tgz"), []member{
		{"Takeout/Google Fotos/Fotos de 2023/IMG_9.jpg", "foto-nueve"},
		{"Takeout/Google Fotos/Fotos de 2023/IMG_9.jpg.json", sidecar("IMG_9.jpg", day(2023, 2, 3))},
		{"Takeout/Google Fotos/Fotos de 2023/sin-json.png", "foto-sin-json"},
		{"Takeout/Correo/Todo el correo.mbox", msgOne},
		{"Takeout/YouTube y YouTube Music/historial/historial de reproducciones.json", "[]"},
	})
	h := plugintest.New(t, takeout.Plugin{}, plugin.Config{"path": drop, "importar_correo": "false", "keep_zip": "true"})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(string(h.File("Fotos/2023/2023-02/IMG_9.jpg")), "foto-nueve")
	biff.AssertTrue(h.Has("Fotos/2023/2023-02/IMG_9.jpg.json"))
	biff.AssertEqual(string(h.File("Fotos/2023/sin-fecha/sin-json.png")), "foto-sin-json")
	biff.AssertTrue(h.Has("YouTube/historial/historial de reproducciones.json"))
	biff.AssertTrue(h.Has("Originales/takeout-20250101T100000Z-001.tgz"))
	biff.AssertEqual(len(pathsWith(h, "Correo/", "")), 0) // not chosen
}

func TestTakeoutUnpackedFolder(t *testing.T) {
	dir := t.TempDir()
	biff.AssertNil(os.MkdirAll(filepath.Join(dir, "Takeout", "Keep"), 0o755))
	biff.AssertNil(os.WriteFile(filepath.Join(dir, "Takeout", "Keep", "Nota.json"), []byte(`{"title":"Idea","textContent":"Una idea\nen dos líneas","createdTimestampUsec":1704103200000000}`), 0o644))
	h := plugintest.New(t, takeout.Plugin{}, plugin.Config{"path": dir})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertTrue(strings.Contains(string(h.File("Keep/Nota.md")), "Una idea  \nen dos líneas"))
}

func TestTakeoutValidate(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "otra-cosa.zip")
	writeZip(t, other, []member{{"hola.txt", "hola"}})
	h := plugintest.New(t, takeout.Plugin{}, plugin.Config{"path": other})
	biff.AssertNotNil(h.Validate())
	h.Config["path"] = filepath.Join(dir, "no-existe")
	biff.AssertNotNil(h.Validate())
	h.Config["path"] = dir // a drop folder, empty for now
	biff.AssertNil(h.Validate())

	// A ZIP that is not from Takeout, left in the drop folder, is skipped.
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(len(h.NAS.Paths("principal")), 0)

	var buf bytes.Buffer
	for _, field := range (takeout.Plugin{}).Info().Fields {
		buf.WriteString(field.Name + " ")
	}
	biff.AssertTrue(strings.Contains(buf.String(), "importar_fotos"))
}
