// Package folder holds the two generic integrations: a local folder copied as
// it is, and a file downloaded from a URL. They cover the services that let
// you export to a folder or publish a link (a calendar, a backup of another
// program) without writing an integration for each.
package folder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/fulldump/datame-suitcase/plugin"
)

func init() {
	plugin.Register(Folder{})
	plugin.Register(URL{})
}

// Folder copies a local folder into the NAS.
type Folder struct{}

// Info implements plugin.Plugin.
func (Folder) Info() plugin.Info {
	return plugin.Info{
		Kind:        "carpeta",
		Title:       "Carpeta local",
		Description: "Copia una carpeta de esta máquina al NAS: lo nuevo y lo que cambia. Lo que borres aquí sigue en el NAS.",
		Help: "Sirve para exportaciones que dejas en una carpeta (Google Takeout descomprimido, copias de otros programas) " +
			"o cualquier carpeta que quieras guardar sin montar el NAS. Si un fichero cambia, el NAS guarda la versión anterior.",
		DefaultFolder:     "/Archivo/Carpeta",
		DefaultEveryHours: 6,
		Fields: []plugin.Field{
			{Name: "path", Label: "Carpeta de esta máquina", Type: "path", Required: true, Placeholder: "/home/ana/Exportaciones"},
			{Name: "hidden", Label: "Incluir ficheros ocultos (los que empiezan por punto)", Type: "checkbox", Default: "false"},
		},
	}
}

// Validate implements plugin.Plugin.
func (Folder) Validate(ctx context.Context, env plugin.Env) error {
	info, err := os.Stat(env.Config.Get("path"))
	if err != nil || !info.IsDir() {
		return fmt.Errorf("no encuentro la carpeta %s", env.Config.Get("path"))
	}
	return nil
}

type folderCursor struct {
	Seen map[string]string `json:"seen"` // relative path → size and mtime when it was copied
}

// Sync implements plugin.Plugin.
func (Folder) Sync(ctx context.Context, run *plugin.Run) error {
	var c folderCursor
	if err := run.Load(&c); err != nil {
		return err
	}
	if c.Seen == nil {
		c.Seen = map[string]string{}
	}
	root := run.Config.Get("path")
	hidden := run.Config.Bool("hidden")
	pending := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			run.Logf("No se puede leer %s: %s", p, err)
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if p != root && !hidden && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() { // folders are walked; links and devices are left out
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		signature := fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
		if c.Seen[rel] == signature {
			return nil
		}
		if _, err := run.Out.Write(ctx, plugin.File{Path: rel, ModTime: info.ModTime(), Size: info.Size(), Mutable: true,
			Open: func() (io.ReadCloser, error) { return os.Open(p) }}); err != nil {
			return err
		}
		if _, existed := c.Seen[rel]; !existed {
			run.Item(1)
		}
		c.Seen[rel] = signature
		if pending++; pending%200 == 0 {
			return run.Save(c)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return run.Save(c)
}

// URL downloads one file from a link, each time it changes.
type URL struct{}

// Info implements plugin.Plugin.
func (URL) Info() plugin.Info {
	return plugin.Info{
		Kind:              "url",
		Title:             "Fichero de una dirección web",
		Description:       "Descarga un fichero de una dirección (un calendario .ics, una exportación publicada) cada vez que cambia.",
		Help:              "Pega la dirección del fichero. Si la dirección lleva una clave privada (como las de los calendarios), se guarda cifrada.",
		DefaultFolder:     "/Archivo/Descargas",
		DefaultEveryHours: 24,
		Fields: []plugin.Field{
			{Name: "url", Label: "Dirección del fichero", Type: "password", Secret: true, Required: true, Placeholder: "https://…"},
			{Name: "name", Label: "Nombre del fichero en el NAS", Type: "text", Placeholder: "calendario.ics", Help: "Si lo dejas vacío, se usa el de la dirección."},
			{Name: "dated", Label: "Guardar una copia con fecha cada vez, en vez de reemplazarlo", Type: "checkbox", Default: "false", Help: "Al reemplazar, el NAS también guarda las versiones anteriores."},
		},
	}
}

// Validate implements plugin.Plugin.
func (URL) Validate(ctx context.Context, env plugin.Env) error {
	u, err := url.Parse(env.Config.Get("url"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("la dirección debe empezar por http:// o https://")
	}
	return nil
}

type urlCursor struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	SHA256       string `json:"sha256,omitempty"` // of the last download, for servers without validators
}

// Sync implements plugin.Plugin.
func (URL) Sync(ctx context.Context, run *plugin.Run) error {
	var c urlCursor
	if err := run.Load(&c); err != nil {
		return err
	}
	link := run.Config.Get("url")
	req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
	if err != nil {
		return err
	}
	if c.ETag != "" {
		req.Header.Set("If-None-Match", c.ETag)
	}
	if c.LastModified != "" {
		req.Header.Set("If-Modified-Since", c.LastModified)
	}
	resp, err := run.HTTP.Do(req)
	if err != nil {
		return errors.New("no se pudo descargar: " + strings.ReplaceAll(err.Error(), link, "la dirección"))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		run.Logf("No ha cambiado")
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("la dirección respondió %d", resp.StatusCode)
	}
	// To a temporary file: it may be big, and the writer reads it twice.
	tmp, err := os.CreateTemp(run.CacheDir, "descarga-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hash), resp.Body)
	if err != nil {
		return fmt.Errorf("la descarga se cortó: %w", err)
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if sum == c.SHA256 {
		run.Logf("No ha cambiado")
		c.ETag, c.LastModified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
		return run.Save(c)
	}

	name := plugin.SafeName(run.Config.Get("name"), 120)
	if run.Config.Get("name") == "" {
		u, _ := url.Parse(link)
		name = plugin.SafeName(path.Base(u.Path), 120)
		if name == "fichero" || name == "_" {
			name = "descarga"
		}
	}
	modTime := run.Now()
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		modTime = t
	}
	file := plugin.File{Path: name, ModTime: modTime, Size: size, Mutable: true, Open: func() (io.ReadCloser, error) { return os.Open(tmp.Name()) }}
	if run.Config.Bool("dated") {
		file.Path, file.Mutable = run.Now().Format("2006-01-02")+"-"+name, false
	}
	result, err := run.Out.Write(ctx, file)
	if err != nil {
		return err
	}
	if result == plugin.Created || result == plugin.Updated {
		run.Item(1)
	}
	c.ETag, c.LastModified, c.SHA256 = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), sum
	return run.Save(c)
}
