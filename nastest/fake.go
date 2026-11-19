// Package nastest is a fake Datame NAS for tests: an HTTPS server with its own
// certificate that implements the routes a device token may use, with the same
// answers as node/api.go in Datame (412 with the current size, 409 on a standby
// machine, 507 when the volume is full, 401 for an unknown token).
package nastest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fulldump/box"

	"github.com/fulldump/datame-suitcase/nas"
)

// File is what the fake keeps for each path.
type File struct {
	Data     []byte
	ModTime  int64
	Versions int // how many times it was written
}

// NAS is the fake. Change its fields under Lock to simulate situations.
type NAS struct {
	Server *httptest.Server
	Token  string

	sync.Mutex
	Volumes map[string]map[string]*File // volume → path → file
	Quota   int64                       // bytes per volume, 0 for none
	Standby bool
	Revoked bool
	Puts    int // PUT requests that reached a handler
	Created int // files written for the first time
	Replace int // files replaced
}

// New starts a fake NAS with one volume, «principal».
func New() *NAS {
	n := &NAS{Token: "dm1_abc123_secretsecret", Volumes: map[string]map[string]*File{"principal": {}}}
	b := box.NewBox()
	b.HandleResourceNotFound = func(ctx context.Context) {
		http.Error(box.GetResponse(ctx), "not found", http.StatusNotFound)
	}
	v0 := b.Group("/v0").WithInterceptors(n.auth)
	v0.Handle("GET", "/me", n.me)
	v0.Handle("GET", "/status", n.status)
	v0.Handle("GET", "/files", n.list)
	v0.Handle("GET", "/file", n.get)
	v0.Handle("PUT", "/file", n.put)
	// A key of its own: httptest shares one certificate between all servers,
	// and the tests need NAS boxes that are told apart by their key.
	n.Server = httptest.NewUnstartedServer(b)
	n.Server.TLS = &tls.Config{Certificates: []tls.Certificate{newCertificate()}}
	n.Server.StartTLS()
	return n
}

func newCertificate() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "datame"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Close stops the server.
func (n *NAS) Close() { n.Server.Close() }

// Code is a pairing code for this fake, like the one its web would show.
func (n *NAS) Code() nas.Code {
	return nas.Code{
		Version:   1,
		Addresses: []string{n.Server.URL},
		Pin:       nas.SPKIPin(n.Server.Certificate()),
		Token:     n.Token,
		Name:      "Pruebas",
		Volume:    "principal",
		Folder:    "/Archivo",
	}
}

// File returns a copy of a stored file.
func (n *NAS) File(volume, filePath string) (File, bool) {
	n.Lock()
	defer n.Unlock()
	file, ok := n.Volumes[volume][filePath]
	if !ok {
		return File{}, false
	}
	return *file, true
}

// Paths lists the paths of a volume, sorted.
func (n *NAS) Paths(volume string) []string {
	n.Lock()
	defer n.Unlock()
	out := []string{}
	for p := range n.Volumes[volume] {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (n *NAS) auth(next box.H) box.H {
	return func(ctx context.Context) {
		r, w := box.GetRequest(ctx), box.GetResponse(ctx)
		n.Lock()
		revoked := n.Revoked
		n.Unlock()
		if revoked || r.Header.Get("Authorization") != "Bearer "+n.Token {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		next(ctx)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func (n *NAS) me(w http.ResponseWriter, r *http.Request) {
	n.Lock()
	defer n.Unlock()
	volumes := []nas.Volume{}
	for id, files := range n.Volumes {
		used := int64(0)
		for _, f := range files {
			used += int64(len(f.Data))
		}
		volumes = append(volumes, nas.Volume{ID: id, Name: strings.ToUpper(id[:1]) + id[1:], Quota: n.Quota, Used: used})
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].ID < volumes[j].ID })
	writeJSON(w, map[string]any{"id": "u1", "name": "ana", "admin": false, "volumes": volumes, "active": !n.Standby})
}

func (n *NAS) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
}

// clean mirrors vault.CleanPath in Datame.
func clean(p string) (string, bool) {
	c := path.Clean("/" + p)
	return c, p != "" && c != "/"
}

func (n *NAS) volume(w http.ResponseWriter, r *http.Request) (map[string]*File, bool) {
	files, ok := n.Volumes[r.URL.Query().Get("volume")]
	if !ok {
		http.Error(w, "no access to this volume", http.StatusForbidden)
	}
	return files, ok
}

func (n *NAS) list(w http.ResponseWriter, r *http.Request) {
	n.Lock()
	defer n.Unlock()
	files, ok := n.volume(w, r)
	if !ok {
		return
	}
	// Like Datame, the whole volume, whatever the path.
	out := []nas.FileInfo{}
	for p, f := range files {
		out = append(out, nas.FileInfo{Path: p, Size: int64(len(f.Data)), ModTime: f.ModTime})
	}
	writeJSON(w, out)
}

func (n *NAS) get(w http.ResponseWriter, r *http.Request) {
	n.Lock()
	defer n.Unlock()
	files, ok := n.volume(w, r)
	if !ok {
		return
	}
	p, valid := clean(r.URL.Query().Get("path"))
	file, exists := files[p]
	if !valid || !exists {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(file.Data)))
	w.Write(file.Data)
}

func (n *NAS) put(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	n.Lock()
	defer n.Unlock()
	n.Puts++
	files, ok := n.volume(w, r)
	if !ok {
		return
	}
	p, valid := clean(r.URL.Query().Get("path"))
	if !valid {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if n.Standby {
		http.Error(w, "this machine is waiting to take over", http.StatusConflict)
		return
	}
	modTime := time.Now().Unix()
	if value := r.URL.Query().Get("mtime"); value != "" {
		if modTime, err = strconv.ParseInt(value, 10, 64); err != nil {
			http.Error(w, "invalid mtime", http.StatusBadRequest)
			return
		}
	}
	current, exists := files[p]
	if r.URL.Query().Has("base") && r.URL.Query().Get("base") == "none" && exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		json.NewEncoder(w).Encode(map[string]any{"exists": true, "version": "v", "size": len(current.Data), "mtime": current.ModTime})
		return
	}
	if n.Quota > 0 {
		used := int64(len(data))
		for other, f := range files {
			if other != p {
				used += int64(len(f.Data))
			}
		}
		if used > n.Quota {
			http.Error(w, "the volume is full", http.StatusInsufficientStorage)
			return
		}
	}
	if exists {
		current.Data, current.ModTime = data, modTime
		current.Versions++
		n.Replace++
	} else {
		files[p] = &File{Data: data, ModTime: modTime, Versions: 1}
		n.Created++
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"path": p, "size": len(data), "mtime": modTime})
}
