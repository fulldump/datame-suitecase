package nas_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/nas"
	"github.com/fulldump/datame-suitcase/nastest"
)

func TestParseCode(t *testing.T) {
	code := nas.Code{Version: 1, Addresses: []string{"https://192.168.1.140:4040"}, Pin: "AB" + string(make([]byte, 0)) + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd", Token: "dm1_x_y", Name: "PC", Volume: "principal", Folder: "/Archivo"}
	text := code.Encode()
	// Pasted from a chat, broken in lines and with spaces around.
	parsed, err := nas.ParseCode("  " + text[:20] + "\n" + text[20:] + " \n")
	biff.AssertNil(err)
	biff.AssertEqual(parsed.Addresses, code.Addresses)
	biff.AssertEqual(parsed.Pin, "ab0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd")
	biff.AssertEqual(parsed.Folder, "/Archivo")

	_, err = nas.ParseCode("hola")
	biff.AssertNotNil(err)
	_, err = nas.ParseCode(nas.CodePrefix + "!!!")
	biff.AssertNotNil(err)
	bad := code
	bad.Token = "otro"
	_, err = nas.ParseCode(bad.Encode())
	biff.AssertNotNil(err)
	bad = code
	bad.Addresses = []string{"http://192.168.1.140:4040"}
	_, err = nas.ParseCode(bad.Encode())
	biff.AssertNotNil(err)
}

func TestClientTalksToThePinnedNAS(t *testing.T) {
	fake := nastest.New()
	defer fake.Close()
	ctx := context.Background()

	// The first address does not answer: the client goes on to the next.
	code := fake.Code()
	code.Addresses = append([]string{"https://127.0.0.1:1"}, code.Addresses...)
	client := nas.NewClient(code, "")
	me, err := client.Me(ctx)
	biff.AssertNil(err)
	biff.AssertEqual(me.Name, "ana")
	biff.AssertEqual(me.Volumes[0].ID, "principal")
	biff.AssertEqual(client.LastAddress(), fake.Server.URL)

	mtime := time.Unix(1700000000, 0)
	biff.AssertNil(client.PutBytes(ctx, "principal", "/Archivo/a.txt", mtime, []byte("hola"), true))
	err = client.PutBytes(ctx, "principal", "/Archivo/a.txt", mtime, []byte("adios"), true)
	var exists *nas.Exists
	biff.AssertTrue(errors.As(err, &exists))
	biff.AssertEqual(exists.Size, int64(4))
	biff.AssertNil(client.PutBytes(ctx, "principal", "/Archivo/a.txt", mtime, []byte("adios"), false))
	file, _ := fake.File("principal", "/Archivo/a.txt")
	biff.AssertEqual(string(file.Data), "adios")
	biff.AssertEqual(file.ModTime, int64(1700000000))
	biff.AssertEqual(file.Versions, 2)

	data, err := client.Get(ctx, "principal", "/Archivo/a.txt", 100)
	biff.AssertNil(err)
	biff.AssertEqual(string(data), "adios")
	_, err = client.Get(ctx, "principal", "/Archivo/nada", 100)
	biff.AssertTrue(errors.Is(err, nas.ErrNotFound))

	biff.AssertNil(client.PutBytes(ctx, "principal", "/Otra/b.txt", mtime, []byte("b"), true))
	files, err := client.Files(ctx, "principal", "/Archivo")
	biff.AssertNil(err)
	biff.AssertEqual(len(files), 1)

	biff.AssertTrue(errors.Is(client.PutBytes(ctx, "ajeno", "/x", mtime, []byte("x"), true), nas.ErrNoAccess))

	fake.Lock()
	fake.Standby = true
	fake.Unlock()
	biff.AssertTrue(errors.Is(client.PutBytes(ctx, "principal", "/Archivo/c.txt", mtime, []byte("c"), true), nas.ErrStandby))

	fake.Lock()
	fake.Standby, fake.Quota = false, 10
	fake.Unlock()
	biff.AssertTrue(errors.Is(client.PutBytes(ctx, "principal", "/Archivo/big", mtime, make([]byte, 20), true), nas.ErrFull))

	fake.Lock()
	fake.Revoked = true
	fake.Unlock()
	_, err = client.Me(ctx)
	biff.AssertTrue(errors.Is(err, nas.ErrRevoked))
}

func TestClientRefusesAnotherKey(t *testing.T) {
	fake := nastest.New()
	defer fake.Close()
	other := nastest.New()
	defer other.Close()

	code := fake.Code()
	code.Addresses = []string{other.Server.URL} // same token, another machine
	_, err := nas.NewClient(code, "").Me(context.Background())
	biff.AssertTrue(errors.Is(err, nas.ErrWrongNAS))
	other.Lock()
	biff.AssertEqual(other.Puts, 0)
	other.Unlock()
}
