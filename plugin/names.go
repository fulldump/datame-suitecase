package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"unicode"
)

// File names must survive being copied to any disk in decades: plain ASCII
// letters, digits and a few signs, short, and the same every time for the same
// item.

var accents = strings.NewReplacer(
	"á", "a", "à", "a", "ä", "a", "â", "a", "ã", "a", "å", "a",
	"é", "e", "è", "e", "ë", "e", "ê", "e",
	"í", "i", "ì", "i", "ï", "i", "î", "i",
	"ó", "o", "ò", "o", "ö", "o", "ô", "o", "õ", "o",
	"ú", "u", "ù", "u", "ü", "u", "û", "u",
	"ñ", "n", "ç", "c", "ß", "ss", "æ", "ae", "ø", "o", "œ", "oe",
)

// Slug turns a title into a piece of a file name: «¡Hola, Año Nuevo!» → «hola-ano-nuevo».
func Slug(text string, max int) string {
	text = accents.Replace(strings.ToLower(text))
	var b strings.Builder
	dash := false
	for _, r := range text {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= max {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// SafeName keeps a file name readable but harmless: no folders, no odd signs.
func SafeName(name string, max int) string {
	name = accents.Replace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)), r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if len(out) > max {
		// Keep the extension, which tells what the file is.
		ext := ""
		if i := strings.LastIndexByte(out, '.'); i > 0 && len(out)-i <= 10 {
			ext = out[i:]
		}
		out = out[:max-len(ext)] + ext
	}
	if out == "" {
		out = "fichero"
	}
	return out
}

// ShortHash is 10 hex characters of the SHA-256 of a text: enough to tell
// apart the items of one person, stable for ever.
func ShortHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:5])
}

// JSON marshals with indentation and a final newline, as people read it.
func JSON(v any) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err) // only plain data is marshaled here
	}
	return append(data, '\n')
}

var (
	blockTags = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/h[1-6]|/blockquote|/tr|/pre)\s*/?>`)
	itemTags  = regexp.MustCompile(`(?i)<\s*li[^>]*>`)
	anyTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	scripts   = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	blankRuns = regexp.MustCompile(`\n{3,}`)
)

// HTMLToText turns HTML into readable text, keeping paragraphs. It is not a
// Markdown converter: the original HTML is always saved beside it.
func HTMLToText(s string) string {
	s = scripts.ReplaceAllString(s, "")
	s = blockTags.ReplaceAllString(s, "\n\n")
	s = itemTags.ReplaceAllString(s, "\n- ")
	s = anyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	s = strings.Join(lines, "\n")
	s = blankRuns.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
