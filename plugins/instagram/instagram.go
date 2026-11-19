// Package instagram imports the export of Instagram's «Descargar tu
// información» in JSON. The API that let a person read their own posts (Basic
// Display) closed in 2024; the export has everything, and it can be asked for
// again and again: each new one adds what is missing and removes nothing.
package instagram

import (
	"context"
	"regexp"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/internal/meta"
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the Instagram export importer.
type Plugin struct{}

var service = meta.Service{Name: "Instagram", Rules: []meta.Rule{
	{Match: regexp.MustCompile(`(^|/)content/posts_\d+\.json$`), Posts: "publicaciones", Title: "Publicaciones"},
	{Match: regexp.MustCompile(`(^|/)content/stories\.json$`), Posts: "historias", Title: "Historias"},
	{Match: regexp.MustCompile(`(^|/)content/reels\.json$`), Posts: "reels", Title: "Reels"},
	{Match: regexp.MustCompile(`(^|/)comments/[^/]*comments[^/]*\.json$`), List: "comentarios"},
	{Match: regexp.MustCompile(`(^|/)likes/liked_posts\.json$`), List: "me-gusta"},
	{Match: regexp.MustCompile(`(^|/)likes/liked_comments\.json$`), List: "me-gusta-comentarios"},
	{Match: regexp.MustCompile(`(^|/)saved/saved_posts\.json$`), List: "guardados"},
	{Match: regexp.MustCompile(`(^|/)followers_and_following/followers(_\d+)?\.json$`), List: "seguidores"},
	{Match: regexp.MustCompile(`(^|/)followers_and_following/following\.json$`), List: "seguidos"},
}}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:  "instagram",
		Title: "Instagram (Descargar tu información)",
		Description: "Importa tu exportación de Instagram: publicaciones, historias y reels por meses con sus fotos y vídeos, mensajes por conversación, " +
			"comentarios, «me gusta», guardados, seguidores y seguidos.",
		Help: "En Instagram, ve a tu perfil → ☰ → Centro de cuentas → Tu información y permisos → Descargar tu información → Descargar o transferir información. " +
			"Elige tu cuenta de Instagram e «Información completa» (o las partes que quieras), «Descargar en el dispositivo», " +
			"el intervalo «Desde el principio», el formato JSON (no HTML) y la calidad alta de los archivos. " +
			"Tarda de horas a días; llega un aviso para descargar uno o varios ZIP. Déjalos en la carpeta que indiques aquí: " +
			"cada exportación nueva añade lo que falte, y lo que borres en Instagram sigue en la copia.",
		DefaultFolder:     "/Archivo/Instagram",
		DefaultEveryHours: 24,
		Fields: []plugin.Field{
			{Name: "path", Label: "Carpeta donde dejas los ZIP (o uno, o la exportación descomprimida)", Type: "path", Required: true, Placeholder: "/home/ana/Descargas/Instagram",
				Help: "Si la exportación llega en varios ZIP, déjalos todos en la misma carpeta."},
		},
	}
}

// Validate implements plugin.Plugin.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error { return service.Validate(ctx, env) }

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error { return service.Sync(ctx, run) }
