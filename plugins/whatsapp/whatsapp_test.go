package whatsapp_test

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
	"github.com/fulldump/datame-suitcase/plugins/whatsapp"
	"github.com/fulldump/datame-suitcase/plugintest"
)

const lrm = "\u200e"

// writeZip makes a synthetic export; files maps names inside to contents.
// Each one gets a later modification time so they import in order.
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

type stored struct {
	ID      string `json:"id"`
	Date    string `json:"date"`
	Type    string `json:"type"`
	Author  string `json:"author"`
	Text    string `json:"text"`
	Media   string `json:"media"`
	File    string `json:"file"`
	Omitted bool   `json:"media_omitted"`
}

func month(t *testing.T, h *plugintest.Harness, rel string) []stored {
	t.Helper()
	var list []stored
	biff.AssertNil(json.Unmarshal(h.File(rel), &list))
	return list
}

func texts(list []stored) []string {
	out := []string{}
	for _, m := range list {
		out = append(out, m.Text)
	}
	return out
}

func TestWhatsAppAndroidSpanish(t *testing.T) {
	drop := t.TempDir()
	chat := strings.Join([]string{
		"18/11/26, 9:00 - Los mensajes y las llamadas están cifrados de extremo a extremo. Nadie fuera de este chat puede leerlos.",
		"18/11/26, 9:01 - Ana Pérez: Hola" + lrm,
		"¿qué tal?",
		"18/11/26, 9:02 - Yo: ok",
		"18/11/26, 9:02 - Yo: ok",
		"19/11/26, 14:03 - Ana Pérez: " + lrm + "IMG-20261119-WA0001.jpg (archivo adjunto)",
		"Mira esto",
		"19/11/26, 14:04 - Ana Pérez cambió el asunto a \"Viaje: Roma\"",
		"30/10/26, 23:59 - Yo: del mes pasado",
	}, "\n")
	writeZip(t, filepath.Join(drop, "Chat de WhatsApp con Ana Pérez.zip"), map[string]string{
		"Chat de WhatsApp con Ana Pérez.txt": chat,
		"IMG-20261119-WA0001.jpg":            "jpeg",
	}, 2*time.Hour)

	h := plugintest.New(t, whatsapp.Plugin{}, plugin.Config{"path": drop})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 7)

	nov := month(t, h, "chats/ana-perez/2026/2026-11.json")
	biff.AssertEqual(texts(nov), []string{
		"Los mensajes y las llamadas están cifrados de extremo a extremo. Nadie fuera de este chat puede leerlos.",
		"Hola\n¿qué tal?", "ok", "ok", "Mira esto", "Ana Pérez cambió el asunto a \"Viaje: Roma\"",
	})
	biff.AssertEqual(nov[0].Type, "system")
	biff.AssertEqual(nov[1].Date, "2026-11-18T09:01:00")
	biff.AssertEqual(nov[1].Author, "Ana Pérez")
	biff.AssertTrue(nov[2].ID != nov[3].ID) // two «ok» in one minute are two messages
	biff.AssertEqual(nov[4].File, "media/2026-11/IMG-20261119-WA0001.jpg")
	biff.AssertEqual(nov[5].Type, "system")
	biff.AssertEqual(string(h.File("chats/ana-perez/media/2026-11/IMG-20261119-WA0001.jpg")), "jpeg")
	md := string(h.File("chats/ana-perez/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(md, "# Ana Pérez: noviembre de 2026"))
	biff.AssertTrue(strings.Contains(md, "**Ana Pérez** · 2026-11-18 09:01\n\nHola  \n¿qué tal?"))
	biff.AssertTrue(strings.Contains(md, "![](../media/2026-11/IMG-20261119-WA0001.jpg)"))
	biff.AssertEqual(texts(month(t, h, "chats/ana-perez/2026/2026-10.json")), []string{"del mes pasado"})

	// The same again: nothing is read or written.
	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A newer export, as a .txt without media, that overlaps the old one: the
	// first messages fell out of it (or were deleted), the photo is
	// «<Multimedia omitido>», and there is one new message.
	newer := strings.Join([]string{
		"18/11/26, 9:02 - Yo: ok",
		"18/11/26, 9:02 - Yo: ok",
		"19/11/26, 14:03 - Ana Pérez: <Multimedia omitido>",
		"Mira esto",
		"19/11/26, 14:04 - Ana Pérez cambió el asunto a \"Viaje: Roma\"",
		"19/11/26, 20:15 - Ana Pérez: ¡Nuevo!",
	}, "\n")
	file := filepath.Join(drop, "Chat de WhatsApp con Ana Pérez (1).txt")
	biff.AssertNil(os.WriteFile(file, []byte(newer), 0o644))
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	nov = month(t, h, "chats/ana-perez/2026/2026-11.json")
	biff.AssertEqual(texts(nov), []string{
		"Los mensajes y las llamadas están cifrados de extremo a extremo. Nadie fuera de este chat puede leerlos.",
		"Hola\n¿qué tal?", "ok", "ok", "Mira esto", "Ana Pérez cambió el asunto a \"Viaje: Roma\"", "¡Nuevo!",
	})
	biff.AssertEqual(nov[4].File, "media/2026-11/IMG-20261119-WA0001.jpg") // the photo saved before stays
	file2, _ := h.NAS.File("principal", "/Archivo/chats/ana-perez/2026/2026-10.json")
	biff.AssertEqual(file2.Versions, 1)
}

func TestWhatsAppIPhoneEnglish(t *testing.T) {
	drop := t.TempDir()
	chat := strings.Join([]string{
		"[11/18/26, 9:00:00 AM] Ana: " + lrm + "Messages and calls are end-to-end encrypted. No one outside of this chat can read them.",
		"[11/18/26, 9:01:05 AM] Ana: Hello",
		"there",
		lrm + "[11/19/26, 2:03:05 PM] Ana: " + lrm + "<attached: 00000003-PHOTO-2026-11-19-14-03-05.jpg>",
		"[11/19/26, 2:04:00 PM] Me: " + lrm + "image omitted",
		lrm + "[11/19/26, 2:05:00 PM] Ana: " + lrm + "Invoice.pdf • 2 pages " + lrm + "<attached: 00000004-Invoice.pdf>",
		"[11/19/26, 12:30:00 AM] Me: late",
	}, "\n")
	writeZip(t, filepath.Join(drop, "WhatsApp Chat - Ana.zip"), map[string]string{
		"_chat.txt":                              chat,
		"00000003-PHOTO-2026-11-19-14-03-05.jpg": "photo",
		"00000004-Invoice.pdf":                   "pdf",
	}, 3*time.Hour)

	h := plugintest.New(t, whatsapp.Plugin{}, plugin.Config{"path": drop})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 6)

	nov := month(t, h, "chats/ana/2026/2026-11.json")
	biff.AssertEqual(len(nov), 6)
	biff.AssertEqual(nov[0].Type, "system")
	biff.AssertEqual(nov[1].Text, "Hello\nthere")
	biff.AssertEqual(nov[1].Date, "2026-11-18T09:01:05")
	biff.AssertEqual(nov[2].Date, "2026-11-19T00:30:00") // 12:30 AM
	biff.AssertEqual(nov[3].Date, "2026-11-19T14:03:05")
	biff.AssertEqual(nov[3].Media, "PHOTO-2026-11-19-14-03-05.jpg")
	biff.AssertEqual(nov[3].File, "media/2026-11/PHOTO-2026-11-19-14-03-05.jpg")
	biff.AssertTrue(nov[4].Omitted)
	biff.AssertEqual(nov[5].File, "media/2026-11/20261119-140500-Invoice.pdf")
	biff.AssertEqual(nov[5].Text, "")
	md := string(h.File("chats/ana/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(md, "[fichero: 20261119-140500-Invoice.pdf](../media/2026-11/20261119-140500-Invoice.pdf)"))
	biff.AssertTrue(strings.Contains(md, "_(fichero no incluido en la exportación)_"))

	// A newer export of the same chat numbers the files differently: nothing
	// new is written for them, and nothing repeats.
	newer := chat + "\n[11/20/26, 8:00:00 AM] Ana: " + lrm + "<attached: 00000009-PHOTO-2026-11-20-08-00-00.jpg>"
	newer = strings.ReplaceAll(newer, "00000003-", "00000007-")
	newer = strings.ReplaceAll(newer, "00000004-", "00000008-")
	writeZip(t, filepath.Join(drop, "WhatsApp Chat - Ana (1).zip"), map[string]string{
		"_chat.txt":                              newer,
		"00000007-PHOTO-2026-11-19-14-03-05.jpg": "photo",
		"00000008-Invoice.pdf":                   "pdf",
		"00000009-PHOTO-2026-11-20-08-00-00.jpg": "photo 2",
	}, time.Hour)
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertEqual(len(month(t, h, "chats/ana/2026/2026-11.json")), 7)
	media := 0
	for _, p := range h.NAS.Paths("principal") {
		if strings.Contains(p, "/media/") {
			media++
		}
	}
	biff.AssertEqual(media, 3)
}

func TestWhatsAppLocales(t *testing.T) {
	drop := t.TempDir()
	// Android in Spanish with a 12-hour clock and the narrow space of newer
	// versions, loose .txt.
	biff.AssertNil(os.WriteFile(filepath.Join(drop, "Chat de WhatsApp con Luis.txt"), []byte(strings.Join([]string{
		"5/11/2026, 2:03\u202fp.\u00a0m. - Luis: tarde",
		"5/11/2026, 12:10 a. m. - Luis: medianoche",
		"5/11/2026, 11:59 a. m. - Luis: mañana",
	}, "\n")), 0o644))
	// Android in American English: month first.
	biff.AssertNil(os.WriteFile(filepath.Join(drop, "WhatsApp Chat with Bea.txt"), []byte(strings.Join([]string{
		"3/4/26, 2:03 PM - Bea: hi",
		"3/4/26, 9:15 AM - Bea: <Media omitted>",
	}, "\n")), 0o644))
	// iPhone in Spanish with a four-digit year and seconds, unpacked folder.
	folder := filepath.Join(drop, "Chat de WhatsApp - Grupo")
	biff.AssertNil(os.MkdirAll(folder, 0o755))
	biff.AssertNil(os.WriteFile(filepath.Join(folder, "_chat.txt"), []byte(strings.Join([]string{
		"[19/11/2026, 14:03:05] Grupo: " + lrm + "Ana creó este grupo",
		"[19/11/2026, 14:04:00] Ana: " + lrm + "imagen omitida",
		"[19/11/2026, 14:05:00] Ana: " + lrm + "<adjunto: 00000001-AUDIO-2026-11-19-14-05-00.opus>",
	}, "\n")), 0o644))
	biff.AssertNil(os.WriteFile(filepath.Join(folder, "00000001-AUDIO-2026-11-19-14-05-00.opus"), []byte("opus"), 0o644))

	h := plugintest.New(t, whatsapp.Plugin{}, plugin.Config{"path": drop})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 8)

	luis := month(t, h, "chats/luis/2026/2026-11.json")
	dates := []string{}
	for _, m := range luis {
		dates = append(dates, m.Date)
	}
	biff.AssertEqual(dates, []string{"2026-11-05T00:10:00", "2026-11-05T11:59:00", "2026-11-05T14:03:00"})

	bea := month(t, h, "chats/bea/2026/2026-03.json")
	biff.AssertEqual(bea[0].Date, "2026-03-04T09:15:00")
	biff.AssertTrue(bea[0].Omitted)

	group := month(t, h, "chats/grupo/2026/2026-11.json")
	biff.AssertEqual(group[0].Type, "system")
	biff.AssertTrue(group[1].Omitted)
	biff.AssertEqual(group[2].File, "media/2026-11/AUDIO-2026-11-19-14-05-00.opus")
	biff.AssertTrue(strings.Contains(string(h.File("chats/grupo/2026/2026-11.md")), "[fichero: AUDIO-2026-11-19-14-05-00.opus]"))
}

func TestWhatsAppRejectsOtherZips(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "otra-cosa.zip")
	writeZip(t, file, map[string]string{"notas.txt": "nada que ver"}, 0)
	h := plugintest.New(t, whatsapp.Plugin{}, plugin.Config{"path": file})
	biff.AssertNotNil(h.Validate())
	h.Config["path"] = filepath.Join(dir, "no-existe")
	biff.AssertNotNil(h.Validate())
}
