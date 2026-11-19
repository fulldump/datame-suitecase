// Package meta reads the exports of Meta's «Descargar tu información» in
// JSON (Instagram and Facebook share the format) and writes them to the NAS:
// posts by month, messages by thread and month, lists as JSON and CSV, and
// every other JSON file as it came, with the text repaired.
package meta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fulldump/datame-suitcase/plugin"
)

// Meta writes non-ASCII text in its JSON as mojibake: each byte of the UTF-8
// is escaped as if it were a Latin-1 character, so «é» arrives as «Ã©» and
// emoji as four odd letters. FixText undoes it when, and only when, the
// string is made only of such characters and its bytes are valid UTF-8; a
// real «é» on its own is not valid UTF-8 as a byte and is left alone.
func FixText(s string) string {
	high := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r > 0xff {
			return s
		}
		if r >= 0x80 {
			high = true
		}
		i += size
	}
	if !high {
		return s
	}
	b := make([]byte, 0, len(s))
	for _, r := range s {
		b = append(b, byte(r))
	}
	if !utf8.Valid(b) {
		return s
	}
	return string(b)
}

// Fix repairs every string of a decoded JSON value, keys included.
func Fix(v any) any {
	switch t := v.(type) {
	case string:
		return FixText(t)
	case []any:
		for i := range t {
			t[i] = Fix(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, value := range t {
			out[FixText(k)] = Fix(value)
		}
		return out
	}
	return v
}

// Decode reads Meta JSON, keeping numbers exact, and repairs its text.
func Decode(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return Fix(v), nil
}

// ---- Reading the generic shapes ----

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return ""
}

func number(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

// Records finds the list of items in a file: the file itself when it is a
// list, or the one list of objects it wraps ({"ig_stories": […]}).
func Records(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	m := asMap(v)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if l, ok := m[k].([]any); ok && len(l) > 0 {
			if _, isObject := l[0].(map[string]any); isObject {
				return l
			}
		}
	}
	return nil
}

func unix(seconds float64) time.Time {
	return time.Unix(int64(seconds), 0).UTC()
}

// TimeOf finds when an item happened, wherever this kind of item keeps it.
func TimeOf(v any) time.Time {
	m := asMap(v)
	if m == nil {
		return time.Time{}
	}
	if ms, ok := number(m["timestamp_ms"]); ok {
		return time.UnixMilli(int64(ms)).UTC()
	}
	for _, k := range []string{"creation_timestamp", "timestamp", "taken_at"} {
		if s, ok := number(m[k]); ok && s > 0 {
			return unix(s)
		}
	}
	for _, k := range []string{"media", "string_list_data", "data", "attachments"} {
		for _, item := range asList(m[k]) {
			if t := TimeOf(item); !t.IsZero() {
				return t
			}
		}
	}
	if sm := asMap(m["string_map_data"]); sm != nil {
		keys := make([]string, 0, len(sm))
		for k := range sm {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if t := TimeOf(sm[k]); !t.IsZero() {
				return t
			}
		}
	}
	for _, k := range []string{"comment", "reaction", "post"} {
		if t := TimeOf(m[k]); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// textOf returns the words of a post: what was written («post» in Facebook,
// «title» in Instagram) and, apart, Facebook's line about what was done
// («Ana ha actualizado su estado»).
func textOf(v any) (text, header string) {
	m := asMap(v)
	posts := []string{}
	for _, item := range asList(m["data"]) {
		if p := str(asMap(item)["post"]); p != "" {
			posts = append(posts, p)
		}
	}
	text = strings.Join(posts, "\n\n")
	title := str(m["title"])
	if text == "" {
		text = title
	} else {
		header = title
	}
	if text == "" {
		for _, item := range asList(m["media"]) {
			if t := str(asMap(item)["title"]); t != "" {
				return t, header
			}
		}
	}
	return text, header
}

// links finds the web addresses an item points to.
func links(v any) []string {
	out := []string{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, k := range []string{"url", "link", "href"} {
				if u := str(t[k]); strings.HasPrefix(u, "http") {
					out = append(out, u)
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(v)
	seen := map[string]bool{}
	unique := out[:0]
	for _, u := range out {
		if !seen[u] {
			seen[u] = true
			unique = append(unique, u)
		}
	}
	return unique
}

// mediaObjects finds the objects that point to a file of the export.
func mediaObjects(v any) []map[string]any {
	out := []map[string]any{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if u := str(t["uri"]); u != "" && !strings.Contains(u, "://") {
				out = append(out, t)
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}

// localKey is where the importer notes, inside a media object, where it put
// the file on the NAS, so that a month rebuilt later still links it.
const localKey = "datame_file"

func hasLocal(v any) int {
	n := 0
	for _, m := range mediaObjects(v) {
		if str(m[localKey]) != "" {
			n++
		}
	}
	return n
}

// keyOf tells two copies of an item apart from two items: the time and the
// first file, or the time and the text. Meta gives no ids.
func keyOf(v any, t time.Time, extra string) string {
	media := mediaObjects(v)
	if len(media) > 0 {
		return fmt.Sprintf("%d-%s", t.Unix(), plugin.ShortHash(extra+"\x00"+baseName(str(media[0]["uri"]))))
	}
	text, _ := textOf(v)
	if text == "" {
		data, _ := json.Marshal(v) // maps marshal with sorted keys: stable
		text = string(data)
	}
	return fmt.Sprintf("%d-%s", t.Unix(), plugin.ShortHash(extra+"\x00"+text))
}

func baseName(uri string) string {
	if i := strings.LastIndexAny(uri, "/\\"); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}
