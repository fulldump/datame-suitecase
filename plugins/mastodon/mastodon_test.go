package mastodon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/mastodon"
	"github.com/fulldump/datame-suitcase/plugintest"
)

// fakeServer is a tiny Mastodon API with pages of 2, so pagination is used.
type fakeServer struct {
	*httptest.Server
	mutex      sync.Mutex
	statuses   map[string]map[string]any // id → status
	history    map[string][]map[string]any
	favourites []map[string]any
	bookmarks  []map[string]any
	followers  []map[string]any
	following  []map[string]any
	// limited makes the next statuses requests answer 429; retryAfter is sent with it.
	limited    int
	retryAfter string
	// statusCalls counts requests to the timeline, to cut a run in the middle.
	statusCalls  int
	failAtStatus int
	historyCalls int
	tokenSeen    string
}

const token = "secreto-de-prueba"

func newFake(t *testing.T) *fakeServer {
	f := &fakeServer{statuses: map[string]map[string]any{}, history: map[string][]map[string]any{}, retryAfter: "3600"}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.route)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) account(id, acct string) map[string]any {
	return map[string]any{"id": id, "username": acct, "acct": acct, "display_name": "Persona " + acct, "url": f.URL + "/@" + acct}
}

func (f *fakeServer) addStatus(id, created, content string, extra map[string]any) {
	s := map[string]any{
		"id": id, "created_at": created, "content": content, "visibility": "public",
		"url": f.URL + "/@ana/" + id, "uri": f.URL + "/users/ana/statuses/" + id,
		"account": f.account("1", "ana"), "media_attachments": []any{}, "mentions": []any{},
		"favourites_count": 0, "reblogs_count": 0, "edited_at": nil, "spoiler_text": "",
	}
	for k, v := range extra {
		s[k] = v
	}
	f.mutex.Lock()
	f.statuses[id] = s
	f.mutex.Unlock()
}

func less(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// page serves an opaque, offset-based list with a Link header, as favourites are.
func (f *fakeServer) page(w http.ResponseWriter, r *http.Request, list []map[string]any) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("max_id"))
	end := min(offset+2, len(list))
	if offset < len(list) && end < len(list) {
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?limit=40&max_id=%d>; rel="next", <%s%s?min_id=0>; rel="prev"`, f.URL, r.URL.Path, end, f.URL, r.URL.Path))
	}
	if offset >= len(list) {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, list[offset:end])
}

func (f *fakeServer) route(w http.ResponseWriter, r *http.Request) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if strings.HasPrefix(r.URL.Path, "/media/") || strings.HasPrefix(r.URL.Path, "/avatars/") {
		if r.Header.Get("Authorization") != "" && strings.HasPrefix(r.URL.Path, "/avatars/") {
			f.tokenSeen = "avatar"
		}
		w.Write([]byte("bytes of " + r.URL.Path))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "The access token is invalid"})
		return
	}
	w.Header().Set("X-RateLimit-Limit", "300")
	w.Header().Set("X-RateLimit-Remaining", "250")
	w.Header().Set("X-RateLimit-Reset", "2030-01-01T00:00:00.000Z")
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/api/v1/accounts/verify_credentials":
		me := f.account("1", "ana")
		me["avatar"] = f.URL + "/avatars/ana.png"
		me["header"] = f.URL + "/headers/original/missing.png"
		writeJSON(w, me)
	case r.URL.Path == "/api/v1/accounts/1/statuses":
		f.statusCalls++
		if f.limited > 0 || f.failAtStatus == f.statusCalls {
			if f.limited > 0 {
				f.limited--
			}
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", f.retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		ids := []string{}
		for id := range f.statuses {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return less(ids[j], ids[i]) }) // newest first
		out := []map[string]any{}
		if minID := q.Get("min_id"); minID != "" {
			// The 2 right after min_id, returned newest first.
			after := []string{}
			for _, id := range ids {
				if less(minID, id) {
					after = append(after, id)
				}
			}
			if len(after) > 2 {
				after = after[len(after)-2:]
			}
			for _, id := range after {
				out = append(out, f.statuses[id])
			}
		} else {
			maxID := q.Get("max_id")
			for _, id := range ids {
				if (maxID == "" || less(id, maxID)) && len(out) < 2 {
					out = append(out, f.statuses[id])
				}
			}
		}
		writeJSON(w, out)
	case strings.HasPrefix(r.URL.Path, "/api/v1/statuses/") && strings.HasSuffix(r.URL.Path, "/history"):
		f.historyCalls++
		id := strings.Split(r.URL.Path, "/")[4]
		writeJSON(w, f.history[id])
	case r.URL.Path == "/api/v1/favourites":
		f.page(w, r, f.favourites)
	case r.URL.Path == "/api/v1/bookmarks":
		f.page(w, r, f.bookmarks)
	case r.URL.Path == "/api/v1/accounts/1/followers":
		f.page(w, r, f.followers)
	case r.URL.Path == "/api/v1/accounts/1/following":
		f.page(w, r, f.following)
	case r.URL.Path == "/api/v1/lists":
		writeJSON(w, []any{map[string]any{"id": "7", "title": "Amigos", "replies_policy": "list"}})
	case r.URL.Path == "/api/v1/lists/7/accounts":
		f.page(w, r, []map[string]any{f.account("20", "luis@otro.example"), f.account("21", "eva")})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeServer) other(id, acct, content string) map[string]any {
	return map[string]any{"id": id, "created_at": "2024-11-01T10:00:00.000Z", "content": content,
		"url": f.URL + "/@" + acct + "/" + id, "account": f.account("9"+id, acct), "media_attachments": []any{}}
}

func ids(t *testing.T, data []byte) []string {
	var list []struct {
		Status struct {
			ID string `json:"id"`
		} `json:"status"`
	}
	biff.AssertNil(json.Unmarshal(data, &list))
	out := []string{}
	for _, e := range list {
		out = append(out, e.Status.ID)
	}
	return out
}

func contains(t *testing.T, data []byte, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(string(data), w) {
			t.Fatalf("missing %q in:\n%s", w, data)
		}
	}
}

func seed(f *fakeServer) {
	f.addStatus("101", "2024-12-20T10:00:00.000Z", "<p>Hola<br>mundo</p>", nil)
	f.addStatus("102", "2025-01-03T09:00:00.000Z", `<p>Mira <a href="https://ejemplo.org/un/articulo/largo" rel="nofollow"><span class="invisible">https://</span><span class="ellipsis">ejemplo.org/un/arti</span><span class="invisible">culo/largo</span></a> y <a href="https://otro.example/tags/go" class="mention hashtag">#<span>go</span></a></p>`, nil)
	f.addStatus("103", "2025-01-04T09:30:00.000Z", `<p><span class="h-card"><a href="https://otro.example/@luis" class="u-url mention">@<span>luis</span></a></span> de acuerdo</p>`, map[string]any{
		"in_reply_to_id": "55", "in_reply_to_account_id": "20", "spoiler_text": "política", "visibility": "unlisted",
		"mentions": []any{map[string]any{"id": "20", "acct": "luis@otro.example", "url": "https://otro.example/@luis"}},
	})
	f.addStatus("104", "2025-01-05T12:00:00.000Z", "", map[string]any{"reblog": f.other("77", "eva", "<p>Algo que impulsé</p>")})
	f.addStatus("105", "2025-01-06T18:00:00.000Z", "<p>Una foto</p>", map[string]any{
		"favourites_count":  3,
		"media_attachments": []any{map[string]any{"id": "900", "type": "image", "url": f.URL + "/media/original/foto.jpg", "description": "Un gato al sol"}},
	})
	f.favourites = []map[string]any{f.other("201", "eva", "<p>fav uno</p>"), f.other("202", "luis", "<p>fav dos</p>"), f.other("203", "eva", "<p>fav tres</p>")}
	f.bookmarks = []map[string]any{f.other("301", "eva", "<p>para leer luego</p>")}
	f.followers = []map[string]any{f.account("20", "luis@otro.example"), f.account("21", "eva"), f.account("22", "sol")}
	f.following = []map[string]any{f.account("21", "eva")}
}

func newHarness(t *testing.T, f *fakeServer) *plugintest.Harness {
	return plugintest.New(t, mastodon.Plugin{}, plugin.Config{"instance": f.URL, "token": token})
}

func TestMastodonSync(t *testing.T) {
	f := newFake(t)
	seed(f)
	h := newHarness(t, f)
	biff.AssertNil(h.Validate())

	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 5+3+1)

	biff.AssertEqual(ids(t, h.File("publicaciones/2025/2025-01.json")), []string{"102", "103", "104", "105"})
	biff.AssertEqual(ids(t, h.File("publicaciones/2024/2024-12.json")), []string{"101"})
	md := h.File("publicaciones/2025/2025-01.md")
	contains(t, md,
		"# Publicaciones de @ana@127.0.0.1",
		"Mira https://ejemplo.org/un/articulo/largo y [#go](https://otro.example/tags/go)",
		"_En respuesta a @luis@otro.example_",
		"**Aviso de contenido:** política",
		"· no listada",
		"_Impulso de una publicación de @eva",
		"Algo que impulsé",
		"![Un gato al sol](../../media/2025/2025-01/900-foto.jpg)",
		"_Texto alternativo: Un gato al sol_",
		"· 3 favoritos · 0 impulsos",
	)
	contains(t, h.File("publicaciones/2024/2024-12.md"), "Hola\n\nmundo")
	biff.AssertEqual(string(h.File("media/2025/2025-01/900-foto.jpg")), "bytes of /media/original/foto.jpg")
	contains(t, h.File("publicaciones/2025/2025-01.json"), `"local_media": {`, `"900": "media/2025/2025-01/900-foto.jpg"`, `"description": "Un gato al sol"`)
	contains(t, h.File("perfil.json"), `"acct": "ana"`)
	biff.AssertTrue(h.Has("perfil/avatar-" + plugin.ShortHash(f.URL+"/avatars/ana.png") + ".png"))
	biff.AssertEqual(f.tokenSeen, "avatar") // same host as the API, so it may get the token
	contains(t, h.File("favoritos.md"), "fav uno", "fav tres")
	contains(t, h.File("guardados.json"), "para leer luego")
	contains(t, h.File("seguidores.csv"), "Account address,Nombre,URL\n", "luis@otro.example,Persona luis@otro.example,", "sol@127.0.0.1")
	contains(t, h.File("seguidos.json"), `"acct": "eva"`)
	contains(t, h.File("listas.md"), "## Amigos", "@luis@otro.example")

	// Nothing new online: nothing is written.
	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)

	// Online: 101 deleted, 105 edited, 106 new, a new favourite on top.
	f.mutex.Lock()
	delete(f.statuses, "101")
	f.statuses["105"]["content"] = "<p>Una foto de mi gato</p>"
	f.statuses["105"]["edited_at"] = "2025-01-07T08:00:00.000Z"
	f.statuses["105"]["favourites_count"] = 9
	f.history["105"] = []map[string]any{{"content": "<p>Una foto</p>"}, {"content": "<p>Una foto de mi gato</p>"}}
	f.statuses["104"]["reblogs_count"] = 5 // counters going up alone do not rewrite the month
	f.favourites = append([]map[string]any{f.other("204", "sol", "<p>fav nuevo</p>")}, f.favourites...)
	f.mutex.Unlock()
	f.addStatus("106", "2025-02-01T07:00:00.000Z", "<p>Febrero</p>", nil)

	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2)
	biff.AssertEqual(ids(t, h.File("publicaciones/2024/2024-12.json")), []string{"101"}) // kept
	biff.AssertEqual(ids(t, h.File("publicaciones/2025/2025-02.json")), []string{"106"})
	month := h.File("publicaciones/2025/2025-01.json")
	contains(t, month, "Una foto de mi gato", `"edits_of": "2025-01-07T08:00:00.000Z"`, `"900": "media/2025/2025-01/900-foto.jpg"`)
	var entries []struct {
		Edits []json.RawMessage `json:"edits"`
	}
	biff.AssertNil(json.Unmarshal(month, &entries))
	biff.AssertEqual(len(entries[3].Edits), 2)
	contains(t, h.File("publicaciones/2025/2025-01.md"), "Una foto de mi gato", "· editada el 2025-01-07 08:00 (2 versiones en el JSON)")
	biff.AssertEqual(f.historyCalls, 1)
	var favs []struct {
		ID string `json:"id"`
	}
	biff.AssertNil(json.Unmarshal(h.File("favoritos.json"), &favs))
	biff.AssertEqual(len(favs), 4)
	biff.AssertEqual(favs[0].ID, "204")

	// Again: the edit is known, its history is not asked for again.
	puts = h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(f.historyCalls, 1)
	biff.AssertEqual(h.NAS.Puts, puts)

	// The server asks to wait for an hour: the run stops, and the next goes on.
	f.addStatus("107", "2025-02-02T07:00:00.000Z", "<p>siete</p>", nil)
	f.mutex.Lock()
	f.limited = 1
	f.mutex.Unlock()
	r = h.Run()
	biff.AssertNotNil(r.Err)
	biff.AssertTrue(strings.Contains(r.Err.Error(), "próxima ejecución"))
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertEqual(ids(t, h.File("publicaciones/2025/2025-02.json")), []string{"106", "107"})

	// A whole new install over the same folder brings nothing new. (It reads
	// every post again, so the counter that went up would be saved: put it back.)
	f.mutex.Lock()
	f.statuses["104"]["reblogs_count"] = 0
	f.mutex.Unlock()
	h.Cursor, h.Manifest = nil, map[string]string{}
	created, replaced := h.NAS.Created, h.NAS.Replace
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Created, created)
	biff.AssertEqual(h.NAS.Replace, replaced)
	biff.AssertEqual(ids(t, h.File("publicaciones/2024/2024-12.json")), []string{"101"})
}

func TestMastodonFirstRunCutAndResumed(t *testing.T) {
	f := newFake(t)
	seed(f)
	f.failAtStatus = 2 // the second page of the walk back
	h := newHarness(t, f)
	h.Config["favourites"], h.Config["bookmarks"], h.Config["follows"], h.Config["lists"] = "false", "false", "false", "false"

	r := h.Run()
	biff.AssertNotNil(r.Err)
	biff.AssertEqual(r.Items, 2) // the first page is saved
	biff.AssertEqual(ids(t, h.File("publicaciones/2025/2025-01.json")), []string{"104", "105"})

	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 3)
	biff.AssertEqual(ids(t, h.File("publicaciones/2025/2025-01.json")), []string{"102", "103", "104", "105"})
	biff.AssertEqual(ids(t, h.File("publicaciones/2024/2024-12.json")), []string{"101"})
}

func TestMastodonShortRateLimitIsWaited(t *testing.T) {
	f := newFake(t)
	seed(f)
	f.limited, f.retryAfter = 1, "0"
	h := newHarness(t, f)
	h.Config["media"] = "false"
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 9)
	biff.AssertFalse(h.Has("media/2025/2025-01/900-foto.jpg"))
}

func TestMastodonValidate(t *testing.T) {
	f := newFake(t)
	h := plugintest.New(t, mastodon.Plugin{}, plugin.Config{"instance": f.URL, "token": "otro"})
	err := h.Validate()
	biff.AssertNotNil(err)
	biff.AssertTrue(strings.Contains(err.Error(), "token"))
	h.Config["token"] = ""
	biff.AssertNotNil(h.Validate())
	h.Config["token"], h.Config["instance"] = token, "ftp://nada"
	biff.AssertNotNil(h.Validate())
}
