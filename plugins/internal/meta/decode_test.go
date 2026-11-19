package meta

import (
	"testing"

	"github.com/fulldump/biff"
)

// mojibake encodes text the way Meta does: each byte of its UTF-8 becomes a
// character of its own.
func mojibake(s string) string {
	runes := []rune{}
	for _, b := range []byte(s) {
		runes = append(runes, rune(b))
	}
	return string(runes)
}

func TestFixText(t *testing.T) {
	for _, s := range []string{"Café con Ñandú", "¿Vienes? 😀", "Ελληνικά, 日本語", "plain ascii", ""} {
		biff.AssertEqual(FixText(mojibake(s)), s)
	}
	// Text that is right already is left alone: «é» alone is not UTF-8 as a byte.
	biff.AssertEqual(FixText("café"), "café")
	biff.AssertEqual(FixText("señor 😀"), "señor 😀")
}

func TestDecodeFixesEverything(t *testing.T) {
	data := []byte(`{"title":"Caf\u00c3\u00a9","list":[{"Hora de env\u00c3\u00ado":{"value":"\u00c3\u0091"}}],"timestamp_ms":1763560980123}`)
	v, err := Decode(data)
	biff.AssertNil(err)
	m := v.(map[string]any)
	biff.AssertEqual(m["title"], "Café")
	biff.AssertEqual(m["list"].([]any)[0].(map[string]any)["Hora de envío"].(map[string]any)["value"], "Ñ")
	biff.AssertEqual(TimeOf(v).UnixMilli(), int64(1763560980123))
}
