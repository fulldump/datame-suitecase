package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/engine"
	"github.com/fulldump/datame-suitcase/nastest"
	_ "github.com/fulldump/datame-suitcase/plugins/all"
	"github.com/fulldump/datame-suitcase/secret"
)

const feed = `<?xml version="1.0"?><rss version="2.0"><channel><title>B</title>
<item><title>Uno</title><link>https://ejemplo.org/uno</link><guid>1</guid><pubDate>Mon, 01 Jan 2024 10:00:00 +0000</pubDate><description>hola</description></item>
</channel></rss>`

func blog(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, feed) }))
	t.Cleanup(server.Close)
	return server
}

func readAll(t *testing.T, dir string) string {
	var b strings.Builder
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			data, _ := os.ReadFile(p)
			b.Write(data)
		}
		return nil
	})
	return b.String()
}

func TestEngineEndToEnd(t *testing.T) {
	fake := nastest.New()
	defer fake.Close()
	dir := t.TempDir()
	ctx := context.Background()

	e, err := engine.New(engine.Options{Dir: dir})
	biff.AssertNil(err)
	biff.AssertFalse(e.NAS(ctx).Paired)

	_, err = e.Pair(ctx, "no es un código")
	biff.AssertNotNil(err)
	view, err := e.Pair(ctx, fake.Code().Encode())
	biff.AssertNil(err)
	biff.AssertTrue(view.Reachable)
	biff.AssertEqual(view.DefaultFolder, "/Archivo")
	biff.AssertEqual(e.NAS(ctx).Volumes[0].ID, "principal")

	// Validation happens before anything is kept.
	_, err = e.Create(ctx, engine.Input{Kind: "rss", Volume: "principal", Folder: "/Archivo/Blog", Config: map[string]string{}})
	var validation engine.ValidationError
	biff.AssertTrue(errors.As(err, &validation))
	_, err = e.Create(ctx, engine.Input{Kind: "nada"})
	biff.AssertTrue(errors.As(err, &validation))

	server := blog(t)
	item, err := e.Create(ctx, engine.Input{Kind: "rss", Volume: "principal", Folder: "Archivo/Blog/", Enabled: true, Config: map[string]string{"url": server.URL, "images": "true"}})
	biff.AssertNil(err)
	biff.AssertEqual(item.Folder, "/Archivo/Blog")
	biff.AssertEqual(item.EveryHours, 6)
	biff.AssertEqual(item.Service, "Blog (RSS o Atom)")

	item, err = e.RunNow(ctx, item.ID)
	biff.AssertNil(err)
	biff.AssertEqual(item.LastError, "")
	biff.AssertEqual(item.LastItems, 1)
	biff.AssertEqual(item.LastFiles.Created, 3)
	biff.AssertTrue(item.LastOK > 0)
	_, ok := fake.File("principal", "/Archivo/Blog/2024/2024-01-01-uno/index.md")
	biff.AssertTrue(ok)

	item, err = e.RunNow(ctx, item.ID)
	biff.AssertNil(err)
	biff.AssertEqual(item.LastItems, 0)
	biff.AssertEqual(item.TotalItems, 1)
	lines, _ := e.Log(item.ID)
	biff.AssertTrue(len(lines) >= 4)

	// A secret field: kept sealed on disk, kept when the form leaves it empty.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			fmt.Fprint(w, `{"login":"ana"}`)
			return
		}
		fmt.Fprint(w, `{"login":"ana"}`)
	}))
	defer gh.Close()
	secretItem, err := e.Create(ctx, engine.Input{Kind: "github", Volume: "principal", Folder: "/Archivo/GitHub",
		Config: map[string]string{"user": "ana", "token": "ghp_muysecreto", "api": gh.URL, "code": "false"}})
	biff.AssertNil(err)
	biff.AssertEqual(secretItem.SecretsSet, []string{"token"})
	_, hasToken := secretItem.Config["token"]
	biff.AssertFalse(hasToken)
	secretItem, err = e.Update(ctx, secretItem.ID, engine.Input{Name: "Mi GitHub", Volume: "principal", Folder: "/Archivo/GitHub",
		Config: map[string]string{"user": "ana", "token": "", "api": gh.URL, "code": "false"}})
	biff.AssertNil(err)
	biff.AssertEqual(secretItem.SecretsSet, []string{"token"})
	biff.AssertEqual(secretItem.Name, "Mi GitHub")

	onDisk := readAll(t, dir)
	biff.AssertFalse(strings.Contains(onDisk, "ghp_muysecreto"))
	biff.AssertFalse(strings.Contains(onDisk, fake.Token))
	info, err := os.Stat(filepath.Join(dir, "clave"))
	biff.AssertNil(err)
	biff.AssertEqual(info.Mode().Perm(), os.FileMode(0o600))

	// Everything is there after a restart.
	again, err := engine.New(engine.Options{Dir: dir})
	biff.AssertNil(err)
	biff.AssertEqual(len(again.List()), 2)
	reloaded, _ := again.Get(item.ID)
	biff.AssertEqual(reloaded.TotalItems, 1)
	biff.AssertTrue(again.NAS(ctx).Reachable)
	reloaded, err = again.RunNow(ctx, item.ID)
	biff.AssertNil(err)
	biff.AssertEqual(reloaded.LastFiles.Created, 0)

	// With another key file the credentials cannot be read, and it says so.
	_, err = engine.New(engine.Options{Dir: dir, KeyFile: filepath.Join(t.TempDir(), "otra")})
	biff.AssertTrue(errors.Is(err, secret.ErrBadKey))

	biff.AssertNil(again.Delete(ctx, secretItem.ID))
	biff.AssertEqual(len(again.List()), 1)
	biff.AssertTrue(errors.Is(again.Delete(ctx, "nada"), engine.ErrNotFound))
}

func TestEngineReportsNASProblems(t *testing.T) {
	fake := nastest.New()
	defer fake.Close()
	ctx := context.Background()
	e, err := engine.New(engine.Options{Dir: t.TempDir()})
	biff.AssertNil(err)
	_, err = e.Pair(ctx, fake.Code().Encode())
	biff.AssertNil(err)
	item, err := e.Create(ctx, engine.Input{Kind: "rss", Volume: "principal", Folder: "/Blog", Enabled: true, Config: map[string]string{"url": blog(t).URL}})
	biff.AssertNil(err)

	fake.Lock()
	fake.Standby = true
	fake.Unlock()
	item, _ = e.RunNow(ctx, item.ID)
	biff.AssertTrue(strings.Contains(item.LastError, "en espera"))
	// After a failure it tries again within the hour, not in 6 hours.
	biff.AssertTrue(item.NextRun-item.LastStart <= int64(time.Hour/time.Second))

	fake.Lock()
	fake.Standby, fake.Revoked = false, true
	fake.Unlock()
	item, _ = e.RunNow(ctx, item.ID)
	biff.AssertTrue(strings.Contains(item.LastError, "vuelve a emparejarlo"))
	biff.AssertFalse(e.NAS(ctx).Reachable)
}

func TestEngineLoopRunsWhatIsDue(t *testing.T) {
	fake := nastest.New()
	defer fake.Close()
	ctx, cancel := context.WithCancel(context.Background())
	e, err := engine.New(engine.Options{Dir: t.TempDir()})
	biff.AssertNil(err)
	_, err = e.Pair(ctx, fake.Code().Encode())
	biff.AssertNil(err)
	item, err := e.Create(ctx, engine.Input{Kind: "rss", Volume: "principal", Folder: "/Blog", Enabled: true, Config: map[string]string{"url": blog(t).URL}})
	biff.AssertNil(err)

	done := make(chan struct{})
	go func() { e.Loop(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		view, _ := e.Get(item.ID)
		if view.LastEnd > 0 && !view.Running {
			biff.AssertEqual(view.LastError, "")
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop did not run the integration")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}
