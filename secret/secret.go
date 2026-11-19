// Package secret encrypts the credentials this program keeps (service tokens
// and the NAS device token) with AES-256-GCM.
//
// The key lives in a file of its own, readable only by its owner (0600). This
// protects the credentials when the data folder is copied, backed up or synced
// somewhere without the key file; it does not protect them from anyone who can
// read files as the user that runs the program, because the program must be
// able to start alone, without a person typing a password. Keeping the key file
// on another disk (-keyfile) makes a copy of the data folder alone useless.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const prefix = "v1:"

// Box seals and opens secrets with one key.
type Box struct {
	aead cipher.AEAD
}

// ErrBadKey means the secrets were sealed with another key.
var ErrBadKey = errors.New("no se pueden descifrar las credenciales: el fichero de clave no es el que las cifró")

// LoadOrCreate reads the key file, creating it with a new random key the first time.
func LoadOrCreate(keyFile string) (*Box, error) {
	data, err := os.ReadFile(keyFile)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
			return nil, err
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		// O_EXCL: two programs starting at once never end up with different keys.
		file, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := file.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
			file.Close()
			return nil, err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		return New(key)
	}
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(keyFile); err == nil && info.Mode().Perm()&0o077 != 0 {
		log.Printf("AVISO: %s se puede leer por otros usuarios; cámbialo con: chmod 600 %s", keyFile, keyFile)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s no es un fichero de clave válido", keyFile)
	}
	return New(key)
}

// New makes a box with a 32-byte key.
func New(key []byte) (*Box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts. The empty string stays empty, so «no secret» needs no key.
func (b *Box) Seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Open decrypts what Seal made.
func (b *Box) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	encoded, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return "", errors.New("credencial cifrada con un formato desconocido")
	}
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(data) < b.aead.NonceSize() {
		return "", ErrBadKey
	}
	plain, err := b.aead.Open(nil, data[:b.aead.NonceSize()], data[b.aead.NonceSize():], nil)
	if err != nil {
		return "", ErrBadKey
	}
	return string(plain), nil
}
