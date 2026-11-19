// Package plugintest runs integrations in tests against a fake NAS, through
// the real writer, keeping the cursor between runs as the engine does.
package plugintest

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/fulldump/datame-suitcase/nas"
	"github.com/fulldump/datame-suitcase/nastest"
	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/writer"
)

// Harness keeps what lasts between runs of one integration.
type Harness struct {
	T        *testing.T
	NAS      *nastest.NAS
	Client   *nas.Client
	Plugin   plugin.Plugin
	Config   plugin.Config
	Folder   string
	Cursor   json.RawMessage
	Manifest map[string]string
	CacheDir string
	HTTP     *http.Client
	Now      time.Time
	Log      []string
}

// New starts a fake NAS for an integration.
func New(t *testing.T, p plugin.Plugin, config plugin.Config) *Harness {
	fake := nastest.New()
	t.Cleanup(fake.Close)
	return &Harness{T: t, NAS: fake, Client: nas.NewClient(fake.Code(), ""), Plugin: p, Config: config, Folder: "/Archivo",
		Manifest: map[string]string{}, CacheDir: t.TempDir(), HTTP: http.DefaultClient, Now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// Env is the environment the integration sees.
func (h *Harness) Env() plugin.Env {
	return plugin.Env{Config: h.Config, HTTP: h.HTTP, CacheDir: h.CacheDir, Now: func() time.Time { return h.Now }}
}

// Validate runs the check of the settings.
func (h *Harness) Validate() error { return h.Plugin.Validate(context.Background(), h.Env()) }

// Result is what a run did.
type Result struct {
	Err   error
	Items int
	Stats writer.Stats
}

// Run runs Sync once.
func (h *Harness) Run() Result {
	w := writer.New(h.Client, "principal", h.Folder, h.Manifest, func(f string, a ...any) {})
	logf := func(format string, args ...any) {
		h.Log = append(h.Log, format)
	}
	run := plugin.NewRun(h.Env(), w, h.Cursor, func(c json.RawMessage) error { h.Cursor = c; return nil }, logf)
	err := h.Plugin.Sync(context.Background(), run)
	h.Cursor = run.Cursor()
	h.Manifest = w.Manifest()
	return Result{Err: err, Items: run.Items(), Stats: w.Stats()}
}

// File returns a file written below the folder, or fails the test.
func (h *Harness) File(rel string) []byte {
	h.T.Helper()
	file, ok := h.NAS.File("principal", h.Folder+"/"+rel)
	if !ok {
		h.T.Fatalf("%s was not written; there are: %v", rel, h.NAS.Paths("principal"))
	}
	return file.Data
}

// Has tells whether a file was written.
func (h *Harness) Has(rel string) bool {
	_, ok := h.NAS.File("principal", h.Folder+"/"+rel)
	return ok
}
