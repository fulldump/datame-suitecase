package linkedin_test

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/linkedin"
	"github.com/fulldump/datame-suitcase/plugintest"
)

func writeZip(t *testing.T, file string, files map[string]string, age time.Duration) {
	t.Helper()
	f, err := os.Create(file)
	biff.AssertNil(err)
	z := zip.NewWriter(f)
	for name, content := range files {
		w, err := z.Create(name)
		biff.AssertNil(err)
		w.Write([]byte(content))
	}
	biff.AssertNil(z.Close())
	biff.AssertNil(f.Close())
	when := time.Now().Add(-age)
	biff.AssertNil(os.Chtimes(file, when, when))
}

const connectionsNotes = "Notes:\r\n\"When exporting your connection data, you may notice that some of the email addresses are missing. You will only see email addresses for connections who have allowed their connections to see or download their email address.\"\r\n\r\n"

func export(connections, messages, shares []string) map[string]string {
	return map[string]string{
		"Connections.csv": connectionsNotes + "First Name,Last Name,URL,Email Address,Company,Position,Connected On\r\n" + strings.Join(connections, "\r\n") + "\r\n",
		"messages.csv": "CONVERSATION ID,CONVERSATION TITLE,FROM,SENDER PROFILE URL,TO,RECIPIENT PROFILE URLS,DATE,SUBJECT,CONTENT,FOLDER,ATTACHMENTS\n" +
			strings.Join(messages, "\n") + "\n",
		"Profile.csv": "First Name,Last Name,Maiden Name,Address,Birth Date,Headline,Summary,Industry,Zip Code,Geo Location,Twitter Handles,Websites,Instant Messengers\n" +
			"Ana,Pérez,,,,Ingeniera de datos,\"Me gusta ordenar cosas.\nY guardarlas.\",Software,,\"Sevilla, España\",,,\n",
		"Positions.csv": "Company Name,Title,Description,Location,Started On,Finished On\n" +
			"Ejemplo S.L.,Ingeniera de datos,Pipelines y copias.,Sevilla,Mar 2024,\n" +
			"Otra Empresa,Becaria,,Madrid,Jan 2022,Feb 2024\n",
		"Education.csv":             "School Name,Start Date,End Date,Notes,Degree Name,Activities\nUniversidad de Ejemplo,2018,2022,,Grado en Informática,Club de ajedrez\n",
		"Skills.csv":                "Name\nGo\nSQL\n",
		"Shares.csv":                "Date,ShareLink,ShareCommentary,SharedUrl,MediaUrl,Visibility\n" + strings.Join(shares, "\n") + "\n",
		"Comments.csv":              "Date,Link,Message\n2026-11-03 10:00:00,https://www.linkedin.com/feed/update/urn:li:activity:1,¡Bien!\n",
		"Reactions.csv":             "Date,Type,Link\n2026-11-03 10:01:00,LIKE,https://www.linkedin.com/feed/update/urn:li:activity:1\n",
		"Jobs/Job Applications.csv": "Application Date,Company Name,Job Title\n11/1/26,Ejemplo S.L.,Ingeniera\n",
		"Invitations.csv":           "From,To,Sent At,Message,Direction,inviterProfileUrl,inviteeProfileUrl\nBea Ruiz,Ana Pérez,\"11/2/26, 9:00 AM\",,INCOMING,,\n",
	}
}

const (
	bea    = "Bea,Ruiz,https://www.linkedin.com/in/bea-ruiz,,Ejemplo S.L.,Diseñadora,02 Nov 2026"
	carlos = "Carlos,Gil,https://www.linkedin.com/in/carlos-gil,carlos@ejemplo.org,,,15 Oct 2025"
	msg1   = "c1,,Bea Ruiz,https://www.linkedin.com/in/bea-ruiz,Ana Pérez,https://www.linkedin.com/in/ana,2026-11-02 09:30:00 UTC,,\"¡Hola, Ana!\nGracias por aceptar.\",INBOX,"
	msg2   = "c1,,Ana Pérez,https://www.linkedin.com/in/ana,Bea Ruiz,https://www.linkedin.com/in/bea-ruiz,2026-11-02 10:00:00 UTC,,Un placer,INBOX,"
	post1  = "2026-11-04 08:00:00,https://www.linkedin.com/feed/update/urn:li:share:100,\"Mi primera publicación, con \"\"comillas\"\".\",https://ejemplo.org/articulo,,MEMBER_NETWORK"
)

func count(data []byte) int {
	var items []any
	if err := json.Unmarshal(data, &items); err != nil {
		panic(err)
	}
	return len(items)
}

func TestLinkedInExportImport(t *testing.T) {
	drop := t.TempDir()
	writeZip(t, filepath.Join(drop, "Complete_LinkedInDataExport_11-05-2026.zip"), export([]string{bea, carlos}, []string{msg1, msg2}, []string{post1}), 2*time.Hour)

	h := plugintest.New(t, linkedin.Plugin{}, plugin.Config{"path": drop})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 5) // 2 contacts, 2 messages, 1 post

	cv := string(h.File("perfil.md"))
	biff.AssertTrue(strings.HasPrefix(cv, "# Ana Pérez\n\n**Ingeniera de datos**\n\nUbicación: Sevilla, España · Sector: Software"))
	biff.AssertTrue(strings.Contains(cv, "## Extracto\n\nMe gusta ordenar cosas.  \nY guardarlas."))
	biff.AssertTrue(strings.Contains(cv, "### Ingeniera de datos — Ejemplo S.L.\n\n_Mar 2024 – actualidad · Sevilla_"))
	biff.AssertTrue(strings.Contains(cv, "### Universidad de Ejemplo — Grado en Informática\n\n_2018 – 2022_"))
	biff.AssertTrue(strings.Contains(cv, "## Aptitudes\n\nGo · SQL"))

	contacts := string(h.File("contactos.csv"))
	biff.AssertTrue(strings.HasPrefix(contacts, "First Name,Last Name,URL,Email Address,Company,Position,Connected On\nBea,Ruiz,"))
	md := string(h.File("contactos.md"))
	biff.AssertTrue(strings.Contains(md, "- [Bea Ruiz](https://www.linkedin.com/in/bea-ruiz) — Diseñadora en Ejemplo S.L. · desde 2026-11-02"))
	biff.AssertTrue(strings.Contains(md, "- [Carlos Gil](https://www.linkedin.com/in/carlos-gil) · carlos@ejemplo.org · desde 2025-10-15"))

	folder := ""
	for _, p := range h.NAS.Paths("principal") {
		if strings.HasPrefix(p, "/Archivo/mensajes/bea-ruiz-") && strings.HasSuffix(p, "/2026/2026-11.md") {
			folder = strings.TrimSuffix(strings.TrimPrefix(p, "/Archivo/"), "/2026/2026-11.md")
		}
	}
	biff.AssertTrue(folder != "")
	chat := string(h.File(folder + "/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(chat, "# Bea Ruiz: noviembre de 2026"))
	biff.AssertTrue(strings.Contains(chat, "**Bea Ruiz** · 2026-11-02 09:30 UTC\n\n¡Hola, Ana!  \nGracias por aceptar."))

	posts := string(h.File("publicaciones/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(posts, "Mi primera publicación, con \"comillas\"."))
	biff.AssertTrue(strings.Contains(posts, "<https://ejemplo.org/articulo>"))

	// The CSVs as they came.
	biff.AssertTrue(strings.HasPrefix(string(h.File("originales/Complete_LinkedInDataExport_11-05-2026/Connections.csv")), "Notes:"))
	biff.AssertTrue(h.Has("originales/Complete_LinkedInDataExport_11-05-2026/Invitations.csv"))
	biff.AssertTrue(h.Has("originales/Complete_LinkedInDataExport_11-05-2026/Reactions.csv"))
	biff.AssertTrue(h.Has("originales/Complete_LinkedInDataExport_11-05-2026/Jobs/Job_Applications.csv"))

	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A newer copy, unpacked: Carlos is no longer a contact and the first
	// message was deleted; there is a new contact, message and post.
	dana := "Dana,Sol,https://www.linkedin.com/in/dana-sol,,,,18 Nov 2026"
	msg3 := "c1,,Bea Ruiz,https://www.linkedin.com/in/bea-ruiz,Ana Pérez,https://www.linkedin.com/in/ana,2026-11-18 12:00:00 UTC,,¿Café?,INBOX,"
	post2 := "2026-11-18 08:00:00,https://www.linkedin.com/feed/update/urn:li:share:101,Otra,,,PUBLIC"
	dir := filepath.Join(drop, "Complete_LinkedInDataExport_11-19-2026")
	biff.AssertNil(os.MkdirAll(dir, 0o755))
	for name, content := range export([]string{dana, bea}, []string{msg2, msg3}, []string{post1, post2}) {
		biff.AssertNil(os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
		biff.AssertNil(os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 3)
	contacts = string(h.File("contactos.csv"))
	biff.AssertEqual(strings.Count(contacts, "\n"), 4) // header and three contacts
	biff.AssertTrue(strings.Contains(contacts, "Carlos"))
	biff.AssertEqual(count(h.File(folder+"/2026/2026-11.json")), 3)
	biff.AssertEqual(count(h.File("publicaciones/2026/2026-11.json")), 2)
	biff.AssertTrue(h.Has("originales/Complete_LinkedInDataExport_11-19-2026/Connections.csv"))
}

func TestLinkedInRejectsOtherZips(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "otra-cosa.zip")
	writeZip(t, file, map[string]string{"datos.csv": "a,b\n1,2\n"}, 0)
	h := plugintest.New(t, linkedin.Plugin{}, plugin.Config{"path": file})
	biff.AssertNotNil(h.Validate())
}
