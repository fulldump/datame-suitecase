// Package facebook imports the export of Facebook's «Descargar tu
// información» in JSON: there is no API for a person to read their own posts
// and messages, and the export has them all, with the photos and videos.
// Each new export adds what is missing and removes nothing.
package facebook

import (
	"context"
	"regexp"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/internal/meta"
)

func init() { plugin.Register(Plugin{}) }

// Plugin is the Facebook export importer.
type Plugin struct{}

var service = meta.Service{Name: "Facebook", Rules: []meta.Rule{
	{Match: regexp.MustCompile(`(^|/)posts/your_posts[^/]*\.json$`), Posts: "publicaciones", Title: "Publicaciones"},
	{Match: regexp.MustCompile(`(^|/)posts/album/[^/]+\.json$`), Album: true, Title: "Álbum"},
	{Match: regexp.MustCompile(`(^|/)posts/your_videos\.json$`), Album: true, Title: "Vídeos"},
	{Match: regexp.MustCompile(`(^|/)posts/your_uncategorized_photos\.json$`), Album: true, Title: "Fotos sin álbum"},
	{Match: regexp.MustCompile(`(^|/)comments_and_reactions/comments\.json$`), List: "comentarios"},
	{Match: regexp.MustCompile(`(^|/)comments_and_reactions/(likes_and_reactions|reactions)[^/]*\.json$`), List: "reacciones"},
	{Match: regexp.MustCompile(`(^|/)friends/(your_)?friends\.json$`), List: "amigos"},
}}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		Kind:  "facebook",
		Title: "Facebook (Descargar tu información)",
		Description: "Importa tu exportación de Facebook: publicaciones por meses, fotos y vídeos por álbum, mensajes por conversación, " +
			"comentarios, reacciones y amigos.",
		Help: "En Facebook, ve a Configuración y privacidad → Configuración → Centro de cuentas → Tu información y permisos → Descargar tu información → Descargar o transferir información. " +
			"Elige tu perfil de Facebook e «Información completa» (o las partes que quieras), «Descargar en el dispositivo», " +
			"el intervalo «Desde el principio», el formato JSON (no HTML) y la calidad alta de los archivos. " +
			"Tarda de horas a días; llega un aviso para descargar uno o varios ZIP. Déjalos en la carpeta que indiques aquí: " +
			"cada exportación nueva añade lo que falte, y lo que borres en Facebook sigue en la copia.",
		DefaultFolder:     "/Archivo/Facebook",
		DefaultEveryHours: 24,
		Fields: []plugin.Field{
			{Name: "path", Label: "Carpeta donde dejas los ZIP (o uno, o la exportación descomprimida)", Type: "path", Required: true, Placeholder: "/home/ana/Descargas/Facebook",
				Help: "Si la exportación llega en varios ZIP, déjalos todos en la misma carpeta."},
		},
	}
}

// Validate implements plugin.Plugin.
func (Plugin) Validate(ctx context.Context, env plugin.Env) error { return service.Validate(ctx, env) }

// Sync implements plugin.Plugin.
func (Plugin) Sync(ctx context.Context, run *plugin.Run) error { return service.Sync(ctx, run) }
