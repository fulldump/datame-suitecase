package writer_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/nas"
	"github.com/fulldump/datame-suitcase/nastest"
	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/writer"
)

func TestWriterNeverDuplicates(t *testing.T) {
	fake := nastest.New()
	defer fake.Close()
	client := nas.NewClient(fake.Code(), "")
	ctx := context.Background()
	when := time.Unix(1600000000, 0)

	w := writer.New(client, "principal", "/Archivo/Blog", nil, nil)
	r, err := w.Write(ctx, plugin.File{Path: "2020/post/index.md", ModTime: when, Data: []byte("# Hola\n"), Mutable: true})
	biff.AssertNil(err)
	biff.AssertEqual(r, plugin.Created)
	big := bytes.Repeat([]byte("x"), 1000)
	r, err = w.Write(ctx, plugin.File{Path: "media/a.jpg", ModTime: when, Size: 1000, Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(big)), nil }})
	biff.AssertNil(err)
	biff.AssertEqual(r, plugin.Created)

	// The same writer again: the manifest answers without asking the NAS.
	puts := fake.Puts
	r, _ = w.Write(ctx, plugin.File{Path: "2020/post/index.md", ModTime: when, Data: []byte("# Hola\n"), Mutable: true})
	biff.AssertEqual(r, plugin.Unchanged)
	biff.AssertEqual(fake.Puts, puts)

	// A new writer without manifest (lost state): the 412 path finds them equal.
	fresh := writer.New(client, "principal", "/Archivo/Blog", nil, nil)
	r, err = fresh.Write(ctx, plugin.File{Path: "2020/post/index.md", ModTime: when, Data: []byte("# Hola\n"), Mutable: true})
	biff.AssertNil(err)
	biff.AssertEqual(r, plugin.Unchanged)
	r, _ = fresh.Write(ctx, plugin.File{Path: "media/a.jpg", ModTime: when, Data: big})
	biff.AssertEqual(r, plugin.Unchanged)

	// A mutable file that changed is replaced; the NAS keeps the history.
	r, err = fresh.Write(ctx, plugin.File{Path: "2020/post/index.md", ModTime: when, Data: []byte("# Hola, editado\n"), Mutable: true})
	biff.AssertNil(err)
	biff.AssertEqual(r, plugin.Updated)
	file, _ := fake.File("principal", "/Archivo/Blog/2020/post/index.md")
	biff.AssertEqual(file.Versions, 2)

	// An immutable one with other content is left alone.
	r, err = fresh.Write(ctx, plugin.File{Path: "media/a.jpg", ModTime: when, Data: []byte("other")})
	biff.AssertNil(err)
	biff.AssertEqual(r, plugin.Kept)
	file, _ = fake.File("principal", "/Archivo/Blog/media/a.jpg")
	biff.AssertEqual(len(file.Data), 1000)

	biff.AssertEqual(fresh.Stats(), writer.Stats{Updated: 1, Unchanged: 2, Kept: 1})
	biff.AssertEqual(fake.Created, 2)

	// Nothing escapes the folder.
	_, err = w.Write(ctx, plugin.File{Path: "../../fuera", Data: []byte("x")})
	biff.AssertNil(err) // cleaned to «fuera» inside the folder
	_, ok := fake.File("principal", "/Archivo/Blog/fuera")
	biff.AssertTrue(ok)
	_, err = w.Write(ctx, plugin.File{Path: "", Data: []byte("x")})
	biff.AssertNotNil(err)

	data, err := w.Read(ctx, "media/a.jpg")
	biff.AssertNil(err)
	biff.AssertEqual(len(data), 1000)
	_, err = w.Read(ctx, "no")
	biff.AssertEqual(err, plugin.ErrNotExist)
}
