// Package takeout imports Google Takeout exports: one or several ZIP (or .tgz)
// parts with a «Takeout/<Product>/…» folder inside, with names in the
// language of the account.
//
// Every product goes to its own folder of the NAS in formats that need no
// Google and no Suitcase to be read: photos and videos as they were taken,
// with their date and the JSON Google keeps of each; mail as the original mbox
// and as one .eml per message; Keep notes as JSON and Markdown; calendars and
// contacts as .ics/.vcf with a Markdown summary; everything else as it comes.
//
// Each Takeout is a full snapshot of the account. Importing a newer one only
// writes what is new or changed, and never removes what older ones left: a
// photo deleted in Google stays in the copy.
package takeout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fulldump/datame-suitcase/plugin"
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the Google Takeout importer.
type Plugin struct{}

// product is one of the groups the person can choose to import.
type product struct {
	key   string // field «importar_<key>»
	label string
	out   string // folder in the NAS
}

var products = []product{
	{"fotos", "Google Fotos", "Fotos"},
	{"drive", "Drive", "Drive"},
	{"correo", "Gmail (el correo)", "Correo"},
	{"keep", "Keep (las notas)", "Keep"},
	{"calendario", "Calendario", "Calendario"},
	{"contactos", "Contactos", "Contactos"},
	{"youtube", "YouTube y YouTube Music (tus vídeos, historial y listas)", "YouTube"},
	{"maps", "Maps (sitios guardados y listas)", "Maps"},
	{"ubicaciones", "Historial de ubicaciones (Cronología)", "Ubicaciones"},
	{"chrome", "Chrome", "Chrome"},
	{"otros", "Todo lo demás", "Otros"},
}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	fields := []plugin.Field{
		{Name: "path", Label: "Carpeta donde dejas los ZIP de Takeout (o un ZIP concreto)", Type: "path", Required: true, Placeholder: "/home/ana/Descargas/Takeout",
			Help: "Deja aquí todas las partes de cada exportación (takeout-…-001.zip, -002.zip…). Sirve también un .tgz o la carpeta ya descomprimida."},
		{Name: "keep_zip", Label: "Guardar también los ZIP originales en el NAS", Type: "checkbox", Default: "false",
			Help: "Ocupan mucho (hasta 50 GB cada parte) y todo lo que traen ya queda guardado por productos."},
		{Name: "keep_mbox", Label: "Guardar también el correo en un solo fichero .mbox, como viene", Type: "checkbox", Default: "true",
			Help: "Además de un .eml por correo. Cada exportación nueva lo reemplaza y el NAS guarda el anterior en su historial."},
	}
	for _, p := range products {
		fields = append(fields, plugin.Field{Name: "importar_" + p.key, Label: "Importar " + p.label, Type: "checkbox", Default: "true"})
	}
	return plugin.Info{
		Kind:        "takeout",
		Title:       "Google Takeout (Fotos, Gmail, Drive, Keep…)",
		Description: "Importa las exportaciones de tu cuenta de Google: fotos con su fecha y álbumes, correos uno a uno, Drive, notas de Keep, calendarios, contactos, YouTube y lo demás.",
		Help: "Entra en takeout.google.com con tu cuenta y elige qué quieres exportar (por defecto, todo). " +
			"En el paso siguiente elige «Exportar cada 2 meses durante 1 año», el tipo de archivo .zip y el tamaño de 50 GB. " +
			"Cuando llegue el correo de Google, descarga todas las partes (takeout-…-001.zip, -002.zip…) y déjalas juntas en la carpeta que indiques aquí. " +
			"Cada exportación nueva solo añade lo que falta: nada se duplica y nada se borra de lo que ya está en el NAS. " +
			"Cuando veas que se ha importado, puedes borrar los ZIP de esa carpeta.",
		DefaultFolder:     "/Archivo/Google",
		DefaultEveryHours: 24,
		Fields:            fields,
	}
}

func enabled(config plugin.Config, key string) bool {
	return config.Get("importar_"+key) != "false"
}

// Validate implements plugin.Plugin.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error {
	root := env.Config.Get("path")
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("no encuentro %s", root)
	}
	chosen := false
	for _, p := range products {
		chosen = chosen || enabled(env.Config, p.key)
	}
	if !chosen {
		return errors.New("marca al menos un producto para importar")
	}
	if info.IsDir() {
		return nil // a drop folder may be empty until the first export arrives
	}
	parts, err := findParts(root)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return fmt.Errorf("%s no es un .zip ni un .tgz", filepath.Base(root))
	}
	ok, err := looksLikeTakeout(parts[0])
	if err != nil {
		return fmt.Errorf("%s no se puede abrir: %w", filepath.Base(root), err)
	}
	if !ok {
		return fmt.Errorf("%s no parece una exportación de Google Takeout (falta la carpeta Takeout)", filepath.Base(root))
	}
	return nil
}

type cursor struct {
	Imported map[string]int64  `json:"imported"` // part key → when it was imported
	Photos   map[string]string `json:"photos"`   // content hash → path under Fotos/, to store each photo once
	Mail     map[string]bool   `json:"mail"`     // Message-ID hash → already saved, whatever its label is now
}

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error {
	var c cursor
	if err := run.Load(&c); err != nil {
		return err
	}
	if c.Imported == nil {
		c.Imported = map[string]int64{}
	}
	if c.Photos == nil {
		c.Photos = map[string]string{}
	}
	if c.Mail == nil {
		c.Mail = map[string]bool{}
	}
	parts, err := findParts(run.Config.Get("path"))
	if err != nil {
		return err
	}
	pending := 0
	for _, group := range groupParts(parts) {
		if err := ctx.Err(); err != nil {
			return err
		}
		fresh := []*part{}
		for _, p := range group {
			if c.Imported[p.key] == 0 {
				fresh = append(fresh, p)
			}
		}
		if len(fresh) == 0 {
			continue
		}
		pending++
		if missing := missingPart(group); missing != "" {
			run.Logf("Falta la parte %s de %s: espero a que esté para importarla", missing, group[0].group)
			continue
		}
		err := importGroup(ctx, run, &c, group, fresh)
		var notReady errNotReady
		if errors.As(err, &notReady) {
			run.Logf("%s todavía no se puede abrir (¿se está descargando?). Lo intentaré en la próxima copia", notReady.name)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", group[0].group, err)
		}
		for _, p := range fresh {
			c.Imported[p.key] = run.Now().Unix()
		}
		if err := run.Save(c); err != nil {
			return err
		}
	}
	if pending == 0 {
		run.Logf("No hay exportaciones nuevas que importar")
	}
	return nil
}

// errNotReady is a part that cannot be opened yet, most often because it is
// still being downloaded. It is tried again on the next run.
type errNotReady struct{ name string }

func (e errNotReady) Error() string { return e.name + " no se puede abrir" }

func importGroup(ctx context.Context, run *plugin.Run, c *cursor, group, fresh []*part) error {
	tmp, err := os.MkdirTemp(run.CacheDir, "takeout-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	idx := newIndex(tmp)
	defer idx.close()
	for _, p := range group {
		if err := idx.scan(ctx, p); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errNotReady{name: p.name}
		}
	}
	if !idx.takeout {
		run.Logf("%s no parece de Google Takeout (falta la carpeta Takeout): no se importa", group[0].name)
		return nil
	}
	run.Logf("Importando %s: %d partes nuevas, %d ficheros en total", group[0].group, len(fresh), len(idx.entries))

	x := newImporter(run, c, idx, tmp)
	for _, p := range fresh {
		run.Logf("Leyendo %s", p.name)
		if err := idx.each(ctx, p, func(e *entry) error { return x.handle(ctx, e) }); err != nil {
			return err
		}
	}
	if err := x.finish(ctx); err != nil {
		return err
	}
	x.report()

	if run.Config.Bool("keep_zip") {
		for _, p := range fresh {
			if p.kind == dirPart {
				continue
			}
			info, err := os.Stat(p.path)
			if err != nil {
				return err
			}
			name := p.path
			if _, err := run.Out.Write(ctx, plugin.File{Path: "Originales/" + cleanName(p.name), ModTime: info.ModTime(), Size: info.Size(),
				Open: func() (io.ReadCloser, error) { return os.Open(name) }}); err != nil {
				return err
			}
		}
	}
	return nil
}

// report logs what the export brought, in a few words.
func (x *importer) report() {
	keys := make([]string, 0, len(x.counts))
	for k := range x.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, x.counts[k]))
	}
	if len(parts) > 0 {
		x.run.Logf("Nuevo en el NAS: %s", strings.Join(parts, ", "))
	} else {
		x.run.Logf("Nada nuevo: todo lo de esta exportación ya estaba en el NAS")
	}
}
