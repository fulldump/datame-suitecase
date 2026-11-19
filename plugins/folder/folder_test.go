package folder_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/folder"
	"github.com/fulldump/datame-suitcase/plugintest"
)

func TestFolderCopiesWhatIsNewOrChanged(t *testing.T) {
	src := t.TempDir()
	biff.AssertNil(os.MkdirAll(filepath.Join(src, "fotos", "2024"), 0o755))
	biff.AssertNil(os.MkdirAll(filepath.Join(src, ".cache"), 0o755))
	biff.AssertNil(os.WriteFile(filepath.Join(src, "notas.txt"), []byte("uno"), 0o644))
	biff.AssertNil(os.WriteFile(filepath.Join(src, "fotos", "2024", "a.jpg"), []byte("jpg"), 0o644))
	biff.AssertNil(os.WriteFile(filepath.Join(src, ".cache", "x"), []byte("x"), 0o644))

	h := plugintest.New(t, folder.Folder{}, plugin.Config{"path": src})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2)
	biff.AssertEqual(string(h.File("fotos/2024/a.jpg")), "jpg")
	biff.AssertFalse(h.Has(".cache/x"))

	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A changed file replaces the copy; one removed here stays in the NAS.
	biff.AssertNil(os.WriteFile(filepath.Join(src, "notas.txt"), []byte("dos"), 0o644))
	later := time.Now().Add(time.Minute)
	biff.AssertNil(os.Chtimes(filepath.Join(src, "notas.txt"), later, later))
	biff.AssertNil(os.Remove(filepath.Join(src, "fotos", "2024", "a.jpg")))
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(string(h.File("notas.txt")), "dos")
	biff.AssertTrue(h.Has("fotos/2024/a.jpg"))

	h.Config["path"] = filepath.Join(src, "no-existe")
	biff.AssertNotNil(h.Validate())
}

func TestURLDownloadsWhenItChanges(t *testing.T) {
	var mutex sync.Mutex
	body := "BEGIN:VCALENDAR\nEND:VCALENDAR\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		fmt.Fprint(w, body) // no validators: the integration compares the content
	}))
	defer server.Close()

	h := plugintest.New(t, folder.URL{}, plugin.Config{"url": server.URL + "/privado/abc123/basic.ics", "name": "calendario.ics"})
	biff.AssertNil(h.Validate())
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertEqual(string(h.File("calendario.ics")), body)

	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)

	mutex.Lock()
	body = "BEGIN:VCALENDAR\nX:1\nEND:VCALENDAR\n"
	mutex.Unlock()
	h.Config["dated"] = "true"
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertTrue(h.Has("2026-01-02-calendario.ics"))

	h.Config["url"] = "file:///etc/passwd"
	biff.AssertNotNil(h.Validate())
}
