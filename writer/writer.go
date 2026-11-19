// Package writer puts the files of an integration in its folder of the NAS.
//
// It never duplicates: each file is sent with «create only» (base=none), and
// when the NAS answers that it exists (412) the writer checks whether it is the
// same before doing anything else. A local manifest of what was written, by
// SHA-256, saves even asking the NAS on the next run. The manifest is only a
// shortcut: if it is lost, the 412 path gives the same result, more slowly.
package writer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/fulldump/datame-suitcase/nas"
	"github.com/fulldump/datame-suitcase/plugin"
)

// compareLimit is the biggest file read back from the NAS to compare it when
// it exists with the same size. Bigger ones of the same size are taken as the
// same, which holds for media named after their own id.
const compareLimit = 8 << 20

// NAS is the part of nas.Client the writer uses.
type NAS interface {
	Put(ctx context.Context, volume, filePath string, modTime time.Time, size int64, body func() (io.ReadCloser, error), createOnly bool) error
	Get(ctx context.Context, volume, filePath string, max int64) ([]byte, error)
}

// Stats counts what happened to the files.
type Stats struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Kept      int `json:"kept"`
}

// Writer is a plugin.Output on a folder of a volume.
type Writer struct {
	nas    NAS
	volume string
	folder string
	logf   func(string, ...any)

	mutex    sync.Mutex
	manifest map[string]string // relative path → sha256 of what is on the NAS
	stats    Stats
	dirty    int
	onDirty  func() // called every so many changes, to save the manifest
}

// New makes a writer. manifest may be nil; it is modified in place.
func New(n NAS, volume, folder string, manifest map[string]string, logf func(string, ...any)) *Writer {
	if manifest == nil {
		manifest = map[string]string{}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Writer{nas: n, volume: volume, folder: "/" + strings.Trim(folder, "/"), manifest: manifest, logf: logf}
}

// OnProgress sets a function called every 100 changed files.
func (w *Writer) OnProgress(f func()) { w.onDirty = f }

// Stats returns the counts so far.
func (w *Writer) Stats() Stats {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.stats
}

// Manifest returns a copy of the manifest.
func (w *Writer) Manifest() map[string]string {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	out := make(map[string]string, len(w.manifest))
	for k, v := range w.manifest {
		out[k] = v
	}
	return out
}

// full returns where a relative path goes on the NAS, refusing to leave the folder.
func (w *Writer) full(rel string) (string, string, error) {
	clean := path.Clean("/" + rel)
	if rel == "" || clean == "/" || strings.Contains(rel, "\\") {
		return "", "", fmt.Errorf("ruta no válida: %q", rel)
	}
	return strings.TrimPrefix(clean, "/"), path.Join(w.folder, clean), nil
}

// Read returns a file of the folder.
func (w *Writer) Read(ctx context.Context, rel string) ([]byte, error) {
	_, full, err := w.full(rel)
	if err != nil {
		return nil, err
	}
	data, err := w.nas.Get(ctx, w.volume, full, 256<<20)
	if errors.Is(err, nas.ErrNotFound) {
		return nil, plugin.ErrNotExist
	}
	return data, err
}

func hashOf(file plugin.File) (string, int64, error) {
	if file.Open == nil {
		sum := sha256.Sum256(file.Data)
		return hex.EncodeToString(sum[:]), int64(len(file.Data)), nil
	}
	body, err := file.Open()
	if err != nil {
		return "", 0, err
	}
	defer body.Close()
	h := sha256.New()
	n, err := io.Copy(h, body)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Write implements plugin.Output.
func (w *Writer) Write(ctx context.Context, file plugin.File) (plugin.Result, error) {
	rel, full, err := w.full(file.Path)
	if err != nil {
		return 0, err
	}
	sum, size, err := hashOf(file)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", rel, err)
	}
	if file.Open != nil && file.Size > 0 && size != file.Size {
		return 0, fmt.Errorf("%s cambió mientras se leía", rel)
	}
	w.mutex.Lock()
	known := w.manifest[rel] == sum
	w.mutex.Unlock()
	if known {
		return w.done(rel, sum, plugin.Unchanged), nil
	}

	open := file.Open
	if open == nil {
		data := file.Data
		open = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	}
	modTime := file.ModTime
	if modTime.IsZero() {
		modTime = time.Now()
	}

	err = w.nas.Put(ctx, w.volume, full, modTime, size, open, true)
	var exists *nas.Exists
	if err == nil {
		return w.done(rel, sum, plugin.Created), nil
	}
	if !errors.As(err, &exists) {
		return 0, err
	}

	same, err := w.same(ctx, full, sum, size, exists.Size, file.Mutable)
	if err != nil {
		return 0, err
	}
	if same {
		return w.done(rel, sum, plugin.Unchanged), nil
	}
	if !file.Mutable {
		w.logf("%s ya existe con otro contenido y no se toca", rel)
		return w.done(rel, "", plugin.Kept), nil
	}
	if err := w.nas.Put(ctx, w.volume, full, modTime, size, open, false); err != nil {
		return 0, err
	}
	return w.done(rel, sum, plugin.Updated), nil
}

// same tells whether what is on the NAS has this content.
func (w *Writer) same(ctx context.Context, full, sum string, size, remoteSize int64, mutable bool) (bool, error) {
	if size != remoteSize {
		return false, nil
	}
	if size > compareLimit {
		return !mutable, nil // a big mutable file of the same size is sent again: the NAS keeps both
	}
	data, err := w.nas.Get(ctx, w.volume, full, compareLimit)
	if err != nil {
		return false, err
	}
	remote := sha256.Sum256(data)
	return hex.EncodeToString(remote[:]) == sum, nil
}

func (w *Writer) done(rel, sum string, result plugin.Result) plugin.Result {
	w.mutex.Lock()
	switch result {
	case plugin.Created:
		w.stats.Created++
	case plugin.Updated:
		w.stats.Updated++
	case plugin.Unchanged:
		w.stats.Unchanged++
	case plugin.Kept:
		w.stats.Kept++
	}
	changed := false
	if sum != "" && w.manifest[rel] != sum {
		w.manifest[rel] = sum
		w.dirty++
		changed = w.dirty%100 == 0
	}
	callback := w.onDirty
	w.mutex.Unlock()
	if changed && callback != nil {
		callback()
	}
	return result
}
