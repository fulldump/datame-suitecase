package telegram_test

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/telegram"
	"github.com/fulldump/datame-suitcase/plugintest"
)

func msg(id int, date, from string, text any, extra map[string]any) map[string]any {
	m := map[string]any{"id": id, "type": "message", "date": date, "date_unixtime": "0", "from": from, "from_id": "user1", "text": text}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// fullExport is a synthetic «JSON legible por máquinas» export of two chats.
func fullExport(extra ...map[string]any) map[string]any {
	ana := []any{
		msg(1, "2024-03-01T10:00:00", "Ana", "Hola\n¿qué tal?", nil),
		msg(2, "2024-03-01T10:05:00", "Yo", []any{"Mira ", map[string]any{"type": "text_link", "text": "esto", "href": "https://ejemplo.org"}}, nil),
		msg(3, "2024-04-02T09:00:00", "Ana", "", map[string]any{"photo": "chats/chat_001/photos/photo_1@02-04-2024_09-00-00.jpg"}),
		msg(4, "2024-04-02T09:01:00", "Ana", "", map[string]any{"file": "(File not included. Change data exporting settings to download.)", "media_type": "video_file"}),
	}
	for _, m := range extra {
		ana = append(ana, m)
	}
	return map[string]any{
		"about":                "synthetic",
		"personal_information": map[string]any{"first_name": "Prueba"},
		"chats": map[string]any{"about": "", "list": []any{
			map[string]any{"name": "Ana Pérez", "type": "personal_chat", "id": 111, "messages": ana},
			map[string]any{"name": "", "type": "saved_messages", "id": 222, "messages": []any{
				map[string]any{"id": 9, "type": "service", "date": "2023-12-31T23:59:00", "actor": "Yo", "action": "pin_message", "text": ""},
			}},
		}},
	}
}

func writeDir(t *testing.T, dir string, export map[string]any) {
	biff.AssertNil(os.MkdirAll(filepath.Join(dir, "chats/chat_001/photos"), 0o755))
	data, _ := json.Marshal(export)
	biff.AssertNil(os.WriteFile(filepath.Join(dir, "result.json"), data, 0o644))
	biff.AssertNil(os.WriteFile(filepath.Join(dir, "chats/chat_001/photos/photo_1@02-04-2024_09-00-00.jpg"), []byte("foto"), 0o644))
}

func TestTelegramExportImport(t *testing.T) {
	drop := t.TempDir()
	writeDir(t, filepath.Join(drop, "DataExport_2024-04-10"), fullExport())

	h := plugintest.New(t, telegram.Plugin{}, plugin.Config{"path": drop})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 5)

	march := string(h.File("chats/111-ana-perez/2024/2024-03.md"))
	biff.AssertTrue(strings.Contains(march, "# Ana Pérez: marzo de 2024"))
	biff.AssertTrue(strings.Contains(march, "Hola  \n¿qué tal?"))
	biff.AssertTrue(strings.Contains(march, "Mira [esto](https://ejemplo.org)"))
	april := string(h.File("chats/111-ana-perez/2024/2024-04.md"))
	biff.AssertTrue(strings.Contains(april, "![](../media/2024-04/3-photo_1_02-04-2024_09-00-00.jpg)"))
	biff.AssertTrue(strings.Contains(april, "no incluido"))
	biff.AssertEqual(string(h.File("chats/111-ana-perez/media/2024-04/3-photo_1_02-04-2024_09-00-00.jpg")), "foto")
	biff.AssertTrue(strings.Contains(string(h.File("chats/222-saved-messages/2023/2023-12.md")), "pin message"))
	biff.AssertTrue(h.Has("perfil.json"))

	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A newer export, zipped inside a folder, where the chat was renamed and
	// message 1 was deleted: the chat stays in its folder and keeps message 1.
	newer := fullExport(msg(5, "2024-04-20T12:00:00", "Ana", "Nuevo", nil))
	chats := newer["chats"].(map[string]any)["list"].([]any)
	first := chats[0].(map[string]any)
	first["name"] = "Ana (trabajo)"
	first["messages"] = first["messages"].([]any)[1:]
	zipFile := filepath.Join(drop, "DataExport_2024-05-01.zip")
	f, _ := os.Create(zipFile)
	z := zip.NewWriter(f)
	w, _ := z.Create("DataExport_2024-05-01/result.json")
	data, _ := json.Marshal(newer)
	w.Write(data)
	w, _ = z.Create("DataExport_2024-05-01/chats/chat_001/photos/photo_1@02-04-2024_09-00-00.jpg")
	w.Write([]byte("foto"))
	biff.AssertNil(z.Close())
	f.Close()

	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	var march2 []map[string]any
	biff.AssertNil(json.Unmarshal(h.File("chats/111-ana-perez/2024/2024-03.json"), &march2))
	biff.AssertEqual(len(march2), 2)
	biff.AssertTrue(strings.Contains(string(h.File("chats/111-ana-perez/2024/2024-04.md")), "Nuevo"))
	for _, p := range h.NAS.Paths("principal") {
		biff.AssertFalse(strings.Contains(p, "trabajo"))
	}
}

func TestTelegramNeedsJSON(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "html.zip")
	f, _ := os.Create(file)
	z := zip.NewWriter(f)
	w, _ := z.Create("export/messages.html")
	w.Write([]byte("<html></html>"))
	z.Close()
	f.Close()
	h := plugintest.New(t, telegram.Plugin{}, plugin.Config{"path": file})
	biff.AssertNotNil(h.Validate())
}
