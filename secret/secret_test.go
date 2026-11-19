package secret_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/secret"
)

func TestBox(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sub", "clave")
	box, err := secret.LoadOrCreate(file)
	biff.AssertNil(err)
	info, err := os.Stat(file)
	biff.AssertNil(err)
	biff.AssertEqual(info.Mode().Perm(), os.FileMode(0o600))

	sealed, err := box.Seal("ghp_token")
	biff.AssertNil(err)
	biff.AssertTrue(strings.HasPrefix(sealed, "v1:"))
	biff.AssertFalse(strings.Contains(sealed, "ghp_token"))
	again, _ := box.Seal("ghp_token")
	biff.AssertNotEqual(sealed, again) // a fresh nonce each time

	reopened, err := secret.LoadOrCreate(file)
	biff.AssertNil(err)
	plain, err := reopened.Open(sealed)
	biff.AssertNil(err)
	biff.AssertEqual(plain, "ghp_token")

	empty, _ := box.Seal("")
	biff.AssertEqual(empty, "")

	other, err := secret.LoadOrCreate(filepath.Join(t.TempDir(), "otra"))
	biff.AssertNil(err)
	_, err = other.Open(sealed)
	biff.AssertEqual(err, secret.ErrBadKey)
	_, err = box.Open(sealed[:len(sealed)-4] + "AAAA")
	biff.AssertEqual(err, secret.ErrBadKey)

	biff.AssertNil(os.WriteFile(filepath.Join(t.TempDir(), "x"), nil, 0o600))
	bad := filepath.Join(t.TempDir(), "mala")
	biff.AssertNil(os.WriteFile(bad, []byte("no hex"), 0o600))
	_, err = secret.LoadOrCreate(bad)
	biff.AssertNotNil(err)
}
