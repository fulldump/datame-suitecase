package nas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// What the NAS answers, as errors a person can act on.
var (
	ErrRevoked  = errors.New("el NAS ya no acepta este programa: vuelve a emparejarlo con un código nuevo")
	ErrStandby  = errors.New("esta máquina del NAS está en espera y no escribe; se reintentará más tarde")
	ErrFull     = errors.New("el volumen del NAS está lleno")
	ErrNoAccess = errors.New("este código no da acceso a ese volumen")
	ErrNotFound = errors.New("no existe en el NAS")
	ErrNoNAS    = errors.New("no encuentro el NAS en ninguna de sus direcciones")
	ErrWrongNAS = errors.New("ha respondido un equipo que no es tu NAS: su clave no coincide")
)

// Exists is returned by Put with createOnly when something is already at that path.
type Exists struct {
	Size    int64
	ModTime int64
}

func (e *Exists) Error() string { return fmt.Sprintf("ya existe (%d bytes)", e.Size) }

// Volume is one the token reaches.
type Volume struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Quota int64  `json:"quota"`
	Used  int64  `json:"used"`
}

// Me is who the token belongs to.
type Me struct {
	Name    string   `json:"name"`
	Volumes []Volume `json:"volumes"`
	Active  bool     `json:"active"`
}

// FileInfo is an entry of a listing.
type FileInfo struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
}

// Client talks to one NAS. It is safe for concurrent use.
type Client struct {
	addresses []string
	token     string
	http      *http.Client

	mutex sync.Mutex
	last  string // the address that answered last, tried first
}

// SPKIPin returns the hash that identifies a NAS: SHA-256 of its public key.
// It does not change when the certificate is renewed.
func SPKIPin(certificate *x509.Certificate) string {
	sum := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// NewClient makes a client for the NAS of a code. last is the address that
// worked before, if any.
func NewClient(code Code, last string) *Client {
	pin := strings.ToLower(code.Pin)
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The NAS has its own certificate, which nobody signs, and its address
		// changes at home. What says it is ours is its key, checked below.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return ErrWrongNAS
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if SPKIPin(leaf) != pin {
				return ErrWrongNAS
			}
			return nil
		},
	}
	transport := &http.Transport{
		TLSClientConfig:       tlsConfig,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute, // the NAS commits a big file before answering
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	}
	addresses := append([]string(nil), code.Addresses...)
	return &Client{addresses: addresses, token: code.Token, http: &http.Client{Transport: transport}, last: last}
}

// LastAddress is the address that answered last.
func (c *Client) LastAddress() string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.last
}

func (c *Client) order() []string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	out := []string{}
	if c.last != "" {
		out = append(out, c.last)
	}
	for _, address := range c.addresses {
		if address != c.last {
			out = append(out, address)
		}
	}
	return out
}

// do sends a request to the first address that answers. body may be nil; it is
// called again for each address, so a failed connection can try the next one.
func (c *Client) do(ctx context.Context, method, uri string, size int64, body func() (io.ReadCloser, error)) (*http.Response, error) {
	var wrong bool
	var lastErr error
	for _, address := range c.order() {
		var reader io.ReadCloser
		if body != nil {
			var err error
			if reader, err = body(); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(address, "/")+uri, reader)
		if err != nil {
			if reader != nil {
				reader.Close()
			}
			return nil, err
		}
		if body != nil {
			req.ContentLength = size
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, ErrWrongNAS) {
				wrong = true
			}
			lastErr = err
			continue
		}
		c.mutex.Lock()
		c.last = address
		c.mutex.Unlock()
		return resp, nil
	}
	if wrong {
		return nil, ErrWrongNAS
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNoNAS, lastErr)
	}
	return nil, ErrNoNAS
}

// statusError turns what the NAS answered into an error, reading the message.
func statusError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ErrRevoked
	case http.StatusConflict:
		return ErrStandby
	case http.StatusInsufficientStorage:
		return ErrFull
	case http.StatusForbidden:
		return ErrNoAccess
	case http.StatusNotFound:
		return ErrNotFound
	}
	message, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("el NAS respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
}

func (c *Client) getJSON(ctx context.Context, uri string, value any) error {
	resp, err := c.do(ctx, "GET", uri, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(value)
}

// Me returns the person behind the token and the volumes it reaches.
func (c *Client) Me(ctx context.Context) (Me, error) {
	var me Me
	err := c.getJSON(ctx, "/v0/me", &me)
	return me, err
}

// Status returns the status document of the NAS as it comes.
func (c *Client) Status(ctx context.Context) (map[string]any, error) {
	status := map[string]any{}
	err := c.getJSON(ctx, "/v0/status", &status)
	return status, err
}

// Files lists the files below a folder of a volume. The NAS answers with the
// whole volume (it ignores the path), so the filtering happens here: use it
// sparingly on big volumes.
func (c *Client) Files(ctx context.Context, volume, folder string) ([]FileInfo, error) {
	var all []FileInfo
	if err := c.getJSON(ctx, "/v0/files?"+url.Values{"volume": {volume}, "path": {folder}}.Encode(), &all); err != nil {
		return nil, err
	}
	prefix := strings.TrimRight(folder, "/") + "/"
	out := []FileInfo{}
	for _, file := range all {
		if strings.HasPrefix(file.Path, prefix) {
			out = append(out, file)
		}
	}
	return out, nil
}

// Get reads a file whole, up to max bytes.
func (c *Client) Get(ctx context.Context, volume, filePath string, max int64) ([]byte, error) {
	resp, err := c.do(ctx, "GET", "/v0/file?"+url.Values{"volume": {volume}, "path": {filePath}}.Encode(), 0, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s ocupa más de %d bytes", filePath, max)
	}
	return data, nil
}

// Put stores a file. With createOnly the NAS refuses to replace anything and
// Put returns *Exists; without it the file is replaced and the NAS keeps the
// previous version in its history.
func (c *Client) Put(ctx context.Context, volume, filePath string, modTime time.Time, size int64, body func() (io.ReadCloser, error), createOnly bool) error {
	query := url.Values{"volume": {volume}, "path": {filePath}, "mtime": {strconv.FormatInt(modTime.Unix(), 10)}}
	if createOnly {
		query.Set("base", "none")
	}
	resp, err := c.do(ctx, "PUT", "/v0/file?"+query.Encode(), size, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		io.Copy(io.Discard, resp.Body)
		return nil
	case http.StatusPreconditionFailed:
		current := struct {
			Size    int64 `json:"size"`
			ModTime int64 `json:"mtime"`
		}{}
		json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&current)
		return &Exists{Size: current.Size, ModTime: current.ModTime}
	}
	return statusError(resp)
}

// PutBytes is Put for content already in memory.
func (c *Client) PutBytes(ctx context.Context, volume, filePath string, modTime time.Time, data []byte, createOnly bool) error {
	return c.Put(ctx, volume, filePath, modTime, int64(len(data)), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}, createOnly)
}
