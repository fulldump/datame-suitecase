package takeout

import (
	"strings"
	"testing"

	"github.com/fulldump/biff"
)

func TestFindSidecar(t *testing.T) {
	names := []string{
		"IMG_1.jpg.json",
		"IMG_2.jpg.supplemental-metadata.json",
		"IMG_3.jpg(1).json",
		"IMG_3.jpg.json",
		"Screenshot_20240101-101010_Una aplicación con.json",
		"VID_4.mp4.supplemental-metadata(2).json",
		"IMG_5.jpg.suppl.json",
	}
	files := map[string]*entry{}
	for _, n := range names {
		files[n] = &entry{name: n}
	}
	cases := map[string]string{
		"IMG_1.jpg":         "IMG_1.jpg.json",
		"IMG_1-edited.jpg":  "IMG_1.jpg.json",
		"IMG_1-editado.jpg": "IMG_1.jpg.json",
		"IMG_2.jpg":         "IMG_2.jpg.supplemental-metadata.json",
		"IMG_3.jpg":         "IMG_3.jpg.json",
		"IMG_3(1).jpg":      "IMG_3.jpg(1).json",
		"Screenshot_20240101-101010_Una aplicación con nombre largo.jpg": "Screenshot_20240101-101010_Una aplicación con.json",
		"VID_4(2).mp4": "VID_4.mp4.supplemental-metadata(2).json",
		"IMG_5.jpg":    "IMG_5.jpg.suppl.json",
		"IMG_10.jpg":   "",
		"IMG_3(2).jpg": "",
	}
	for photo, want := range cases {
		got := findSidecar(files, photo)
		name := ""
		if got != nil {
			name = got.name
		}
		if name != want {
			t.Errorf("%s: got %q, want %q", photo, name, want)
		}
	}
}

func TestLabels(t *testing.T) {
	biff.AssertEqual(gmailLabels(`Recibidos,"Viajes, 2024",=?UTF-8?Q?Ca=C3=B1=C3=B3n?=`), []string{"Recibidos", "Viajes, 2024", "Cañón"})
	biff.AssertEqual(pickLabel([]string{"Recibidos", "Importante", "Trabajo"}), "Trabajo")
	biff.AssertEqual(pickLabel([]string{"Abierto", "Recibidos", "Enviados"}), "Enviados")
	biff.AssertEqual(pickLabel([]string{"Categoría: social", "Inbox"}), "Inbox")
	biff.AssertEqual(pickLabel(nil), "Todos")
}

func TestNames(t *testing.T) {
	biff.AssertEqual(cleanName(`Mis "cosas": a/b?`), "Mis _cosas__ a_b_")
	biff.AssertEqual(cleanName(".."), "_")
	long := cleanName(strings.Repeat("ñ", 100) + ".jpg")
	biff.AssertTrue(len(long) <= 150 && strings.HasSuffix(long, "ñ.jpg"))
	biff.AssertEqual(classify("Google Fotos", 0).out, "Fotos")
	biff.AssertEqual(classify("Maps (tus sitios)", 0).out, "Maps/Sitios")
	biff.AssertEqual(classify("Gmail auf Deutsch", hintMbox).key, "correo")
	biff.AssertEqual(classify("Google Pay", 0).out, "Otros/Google Pay")
}
