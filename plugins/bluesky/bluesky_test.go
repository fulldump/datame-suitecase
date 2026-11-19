package bluesky_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
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
	"github.com/fulldump/datame-suitcase/plugins/bluesky"
	"github.com/fulldump/datame-suitcase/plugintest"
)

// ---- A minimal DAG-CBOR and CAR writer, to build synthetic repositories ----

type link []byte // a binary CID

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func (l link) String() string { return "b" + b32.EncodeToString(l) }

func cidFor(codec byte, data []byte) link {
	sum := sha256.Sum256(data)
	return append(link{1, codec, 0x12, 0x20}, sum[:]...)
}

func head(b *bytes.Buffer, major byte, n uint64) {
	switch {
	case n < 24:
		b.WriteByte(major<<5 | byte(n))
	case n < 1<<8:
		b.WriteByte(major<<5 | 24)
		b.WriteByte(byte(n))
	case n < 1<<16:
		b.WriteByte(major<<5 | 25)
		binary.Write(b, binary.BigEndian, uint16(n))
	case n < 1<<32:
		b.WriteByte(major<<5 | 26)
		binary.Write(b, binary.BigEndian, uint32(n))
	default:
		b.WriteByte(major<<5 | 27)
		binary.Write(b, binary.BigEndian, n)
	}
}

func encode(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteByte(0xf6)
	case bool:
		if t {
			b.WriteByte(0xf5)
		} else {
			b.WriteByte(0xf4)
		}
	case int:
		if t >= 0 {
			head(b, 0, uint64(t))
		} else {
			head(b, 1, uint64(-1-t))
		}
	case string:
		head(b, 3, uint64(len(t)))
		b.WriteString(t)
	case []byte:
		head(b, 2, uint64(len(t)))
		b.Write(t)
	case link:
		b.Write([]byte{0xd8, 42})
		head(b, 2, uint64(len(t)+1))
		b.WriteByte(0)
		b.Write(t)
	case []any:
		head(b, 4, uint64(len(t)))
		for _, item := range t {
			encode(b, item)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// DAG-CBOR order: shorter keys first, then bytewise.
		sort.Slice(keys, func(i, j int) bool {
			if len(keys[i]) != len(keys[j]) {
				return len(keys[i]) < len(keys[j])
			}
			return keys[i] < keys[j]
		})
		head(b, 5, uint64(len(t)))
		for _, k := range keys {
			encode(b, k)
			encode(b, t[k])
		}
	default:
		panic(fmt.Sprintf("cannot encode %T", v))
	}
}

func cbor(v any) []byte {
	var b bytes.Buffer
	encode(&b, v)
	return b.Bytes()
}

type block struct {
	cid  link
	data []byte
}

func car(root link, blocks []block) []byte {
	var out bytes.Buffer
	header := cbor(map[string]any{"version": 1, "roots": []any{root}})
	out.Write(binary.AppendUvarint(nil, uint64(len(header))))
	out.Write(header)
	for _, bl := range blocks {
		out.Write(binary.AppendUvarint(nil, uint64(len(bl.cid)+len(bl.data))))
		out.Write(bl.cid)
		out.Write(bl.data)
	}
	return out.Bytes()
}

// ---- Fake PDS ----

const (
	did      = "did:plc:ana123"
	password = "abcd-efgh-ijkl-mnop"
)

type fakePDS struct {
	pds, entry *httptest.Server
	mutex      sync.Mutex

	records  map[string]map[string]any // «collection/rkey» → record
	blobs    map[string][]byte         // CID → data
	blobRev  map[string]int            // CID → revision it was uploaded in
	rev      int
	versions map[int]map[string]bool // revision → its blocks
	root     link
	blocks   []block

	repoCalls int
	since     []string
	blobCalls map[string]int
	lastFull  []byte
	tamper    bool
	failList  int // the next listBlobs calls fail
}

func revName(n int) string { return fmt.Sprintf("3l%011d", n) }

func newFakePDS(t *testing.T) *fakePDS {
	f := &fakePDS{records: map[string]map[string]any{}, blobs: map[string][]byte{}, blobRev: map[string]int{},
		versions: map[int]map[string]bool{}, blobCalls: map[string]int{}}
	f.pds = httptest.NewServer(http.HandlerFunc(f.servePDS))
	t.Cleanup(f.pds.Close)
	f.entry = httptest.NewServer(http.HandlerFunc(f.serveEntry))
	t.Cleanup(f.entry.Close)
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// serveEntry is bsky.social: it only logs in, and says where the PDS is.
func (f *fakePDS) serveEntry(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.Path != "/xrpc/com.atproto.server.createSession" {
		http.NotFound(w, r)
		return
	}
	var body struct{ Identifier, Password string }
	json.NewDecoder(r.Body).Decode(&body)
	if body.Identifier != "ana.test" || body.Password != password {
		writeJSON(w, 401, map[string]string{"error": "AuthenticationRequired", "message": "Invalid identifier or password"})
		return
	}
	writeJSON(w, 200, map[string]any{"accessJwt": "jwt-acceso", "refreshJwt": "jwt-refresco", "handle": "ana.test", "did": did,
		"didDoc": map[string]any{"id": did, "service": []any{map[string]any{"id": "#atproto_pds", "type": "AtprotoPersonalDataServer", "serviceEndpoint": f.pds.URL}}}})
}

func (f *fakePDS) addBlob(data, mimeType string) map[string]any {
	c := cidFor(0x55, []byte(data))
	f.blobs[c.String()] = []byte(data)
	f.blobRev[c.String()] = f.rev + 1 // it goes with the next commit
	return map[string]any{"$type": "blob", "ref": c, "mimeType": mimeType, "size": len(data)}
}

// commit makes a new revision with the records as they are now. The tree is
// one node, with keys compressed against the previous one as the MST does.
func (f *fakePDS) commit() {
	f.rev++
	keys := make([]string, 0, len(f.records))
	for k := range f.records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	blocks := []block{}
	entries := []any{}
	prev := ""
	for _, k := range keys {
		data := cbor(f.records[k])
		c := cidFor(0x71, data)
		blocks = append(blocks, block{c, data})
		p := 0
		for p < len(prev) && p < len(k) && prev[p] == k[p] {
			p++
		}
		entries = append(entries, map[string]any{"p": p, "k": []byte(k[p:]), "v": c, "t": nil})
		prev = k
	}
	node := cbor(map[string]any{"l": nil, "e": entries})
	nodeCID := cidFor(0x71, node)
	blocks = append(blocks, block{nodeCID, node})
	commit := cbor(map[string]any{"did": did, "version": 3, "data": nodeCID, "rev": revName(f.rev), "prev": nil, "sig": []byte("firma")})
	f.root = cidFor(0x71, commit)
	f.blocks = append([]block{{f.root, commit}}, blocks...)
	set := map[string]bool{}
	for _, bl := range f.blocks {
		set[string(bl.cid)] = true
	}
	f.versions[f.rev] = set
}

func (f *fakePDS) servePDS(w http.ResponseWriter, r *http.Request) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if r.Header.Get("Authorization") != "Bearer jwt-acceso" {
		writeJSON(w, 401, map[string]string{"error": "AuthenticationRequired"})
		return
	}
	q := r.URL.Query()
	if q.Get("did") != did {
		writeJSON(w, 400, map[string]string{"error": "RepoNotFound", "message": "Could not find repo"})
		return
	}
	switch r.URL.Path {
	case "/xrpc/com.atproto.sync.getLatestCommit":
		writeJSON(w, 200, map[string]string{"cid": f.root.String(), "rev": revName(f.rev)})
	case "/xrpc/com.atproto.sync.getRepo":
		f.repoCalls++
		blocks := f.blocks
		if since := q.Get("since"); since != "" {
			f.since = append(f.since, since)
			n, _ := strconv.Atoi(strings.TrimPrefix(since, "3l"))
			if old, ok := f.versions[n]; ok {
				blocks = nil
				for _, bl := range f.blocks {
					if !old[string(bl.cid)] {
						blocks = append(blocks, bl)
					}
				}
			}
		}
		data := car(f.root, blocks)
		if q.Get("since") == "" {
			f.lastFull = data
		}
		if f.tamper {
			data = bytes.Replace(data, []byte("Hola"), []byte("Hole"), 1)
		}
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		w.Write(data)
	case "/xrpc/com.atproto.sync.listBlobs":
		if f.failList > 0 {
			f.failList--
			w.Header().Set("RateLimit-Reset", "1900000000")
			writeJSON(w, 429, map[string]string{"error": "RateLimitExceeded", "message": "Rate Limit Exceeded"})
			return
		}
		since := 0
		if s := q.Get("since"); s != "" {
			since, _ = strconv.Atoi(strings.TrimPrefix(s, "3l"))
		}
		ids := []string{}
		for id, rev := range f.blobRev {
			if rev > since {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		// Pages of one, to go through the cursor.
		start := 0
		if c := q.Get("cursor"); c != "" {
			for i, id := range ids {
				if id == c {
					start = i + 1
				}
			}
		}
		page := map[string]any{"cids": []string{}}
		if start < len(ids) {
			page = map[string]any{"cids": ids[start : start+1], "cursor": ids[start]}
		}
		writeJSON(w, 200, page)
	case "/xrpc/com.atproto.sync.getBlob":
		data, ok := f.blobs[q.Get("cid")]
		if !ok {
			writeJSON(w, 400, map[string]string{"error": "BlobNotFound", "message": "Blob not found"})
			return
		}
		f.blobCalls[q.Get("cid")]++
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func post(text, created string, extra map[string]any) map[string]any {
	p := map[string]any{"$type": "app.bsky.feed.post", "text": text, "createdAt": created, "langs": []any{"es"}}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func seed(f *fakePDS) (photo, avatar string) {
	av := f.addBlob("png del avatar", "image/png")
	img := f.addBlob("jpeg del perro", "image/jpeg")
	f.records["app.bsky.actor.profile/self"] = map[string]any{"$type": "app.bsky.actor.profile", "displayName": "Ana", "description": "Hola", "avatar": av}
	f.records["app.bsky.feed.post/3kaaaaaaaaa22"] = post("Hola\nmundo", "2024-12-20T10:00:00.000Z", nil)
	f.records["app.bsky.feed.post/3kbbbbbbbbb22"] = post("Mi perro", "2025-01-04T09:30:00.000Z", map[string]any{
		"embed": map[string]any{"$type": "app.bsky.embed.images", "images": []any{map[string]any{"alt": "Un perro [feliz]", "image": img}}}})
	f.records["app.bsky.feed.post/3kccccccccc22"] = post("Mira ejemplo.org/art...", "2025-01-05T18:00:00.000Z", map[string]any{
		"reply":  map[string]any{"root": map[string]any{"uri": "at://did:plc:luis/app.bsky.feed.post/3kzz", "cid": "bafy"}, "parent": map[string]any{"uri": "at://did:plc:luis/app.bsky.feed.post/3kzz", "cid": "bafy"}},
		"facets": []any{map[string]any{"index": map[string]any{"byteStart": 5, "byteEnd": 23}, "features": []any{map[string]any{"$type": "app.bsky.richtext.facet#link", "uri": "https://ejemplo.org/articulo-largo"}}}},
	})
	f.records["app.bsky.feed.like/3kddddddddd22"] = map[string]any{"$type": "app.bsky.feed.like", "createdAt": "2025-01-06T10:00:00.000Z",
		"subject": map[string]any{"uri": "at://did:plc:luis/app.bsky.feed.post/3kyy", "cid": "bafy"}}
	f.records["app.bsky.feed.repost/3keeeeeeeee22"] = map[string]any{"$type": "app.bsky.feed.repost", "createdAt": "2025-01-07T10:00:00.000Z",
		"subject": map[string]any{"uri": "at://did:plc:eva/app.bsky.feed.post/3kxx", "cid": "bafy"}}
	f.records["app.bsky.graph.follow/3kfffffffff22"] = map[string]any{"$type": "app.bsky.graph.follow", "createdAt": "2025-01-01T10:00:00.000Z", "subject": "did:plc:luis"}
	f.records["app.bsky.graph.list/3kggggggggg22"] = map[string]any{"$type": "app.bsky.graph.list", "name": "Amigos", "purpose": "app.bsky.graph.defs#curatelist", "createdAt": "2025-01-01T10:00:00.000Z"}
	f.records["app.bsky.graph.listitem/3khhhhhhhhh22"] = map[string]any{"$type": "app.bsky.graph.listitem", "subject": "did:plc:eva",
		"list": "at://" + did + "/app.bsky.graph.list/3kggggggggg22", "createdAt": "2025-01-01T10:00:00.000Z"}
	f.commit()
	return img["ref"].(link).String(), av["ref"].(link).String()
}

func uris(t *testing.T, data []byte) []string {
	var list []struct {
		URI string `json:"uri"`
	}
	biff.AssertNil(json.Unmarshal(data, &list))
	out := []string{}
	for _, it := range list {
		out = append(out, it.URI[strings.LastIndex(it.URI, "/")+1:])
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

func TestBlueskySync(t *testing.T) {
	f := newFakePDS(t)
	photo, avatar := seed(f)
	h := plugintest.New(t, bluesky.Plugin{}, plugin.Config{"handle": "@ana.test", "password": password, "pds": f.entry.URL})
	biff.AssertNil(h.Validate())

	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 3+1+1) // posts, like, repost

	// The CAR, exactly as the PDS gave it.
	biff.AssertEqual(h.File("repositorio/"+revName(1)+"-completo.car"), f.lastFull)
	biff.AssertEqual(uris(t, h.File("publicaciones/2025/2025-01.json")), []string{"3kbbbbbbbbb22", "3kccccccccc22"})
	biff.AssertEqual(uris(t, h.File("publicaciones/2024/2024-12.json")), []string{"3kaaaaaaaaa22"})
	month := h.File("publicaciones/2025/2025-01.json")
	contains(t, month, `"$link": "`+photo+`"`, `"`+photo+`": "media/2025/2025-01/`+photo+`.jpg"`, `"alt": "Un perro [feliz]"`)
	md := h.File("publicaciones/2025/2025-01.md")
	contains(t, md,
		"# Publicaciones de @ana.test: enero de 2025",
		"![Un perro (feliz)](../../media/2025/2025-01/"+photo+".jpg)",
		"_Texto alternativo: Un perro (feliz)_",
		"_En respuesta a <https://bsky.app/profile/did:plc:luis/post/3kzz>_",
		"Enlace: <https://ejemplo.org/articulo-largo>",
		"<https://bsky.app/profile/"+did+"/post/3kccccccccc22>",
	)
	contains(t, h.File("publicaciones/2024/2024-12.md"), "Hola  \nmundo")
	biff.AssertEqual(string(h.File("media/2025/2025-01/"+photo+".jpg")), "jpeg del perro")
	biff.AssertEqual(string(h.File("perfil/"+avatar+".png")), "png del avatar")
	contains(t, h.File("perfil.json"), `"displayName": "Ana"`, `"handle": "ana.test"`)
	contains(t, h.File("me-gusta.md"), "https://bsky.app/profile/did:plc:luis/post/3kyy")
	contains(t, h.File("republicaciones.md"), "https://bsky.app/profile/did:plc:eva/post/3kxx")
	contains(t, h.File("seguidos.csv"), "did,desde,perfil\ndid:plc:luis,2025-01-01T10:00:00Z,https://bsky.app/profile/did:plc:luis\n")
	contains(t, h.File("listas.md"), "## Amigos", "https://bsky.app/profile/did:plc:eva")

	// Nothing changed: not even the repository is downloaded.
	puts, calls := h.NAS.Puts, f.repoCalls
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Puts, puts)
	biff.AssertEqual(f.repoCalls, calls)

	// Online: the first post deleted, a new one with a video, a new like.
	f.mutex.Lock()
	delete(f.records, "app.bsky.feed.post/3kaaaaaaaaa22")
	video := f.addBlob("mp4 del gato", "video/mp4")
	f.records["app.bsky.feed.post/3kiiiiiiiii22"] = post("Un vídeo", "2025-02-01T07:00:00.000Z", map[string]any{
		"embed": map[string]any{"$type": "app.bsky.embed.video", "video": video, "alt": "Gato saltando"}})
	f.records["app.bsky.feed.like/3kjjjjjjjjj22"] = map[string]any{"$type": "app.bsky.feed.like", "createdAt": "2025-02-02T10:00:00.000Z",
		"subject": map[string]any{"uri": "at://did:plc:sol/app.bsky.feed.post/3kww", "cid": "bafy"}}
	f.commit()
	f.mutex.Unlock()
	videoID := video["ref"].(link).String()

	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 2)
	biff.AssertEqual(f.since, []string{revName(1)})
	biff.AssertTrue(h.Has("repositorio/" + revName(2) + "-desde-" + revName(1) + ".car"))
	biff.AssertEqual(uris(t, h.File("publicaciones/2024/2024-12.json")), []string{"3kaaaaaaaaa22"}) // kept
	contains(t, h.File("publicaciones/2025/2025-02.md"), "[vídeo](../../media/2025/2025-02/"+videoID+".mp4)", "_Texto alternativo: Gato saltando_")
	biff.AssertEqual(string(h.File("media/2025/2025-02/"+videoID+".mp4")), "mp4 del gato")
	biff.AssertEqual(f.blobCalls, map[string]int{photo: 1, avatar: 1, videoID: 1}) // each blob once
	biff.AssertEqual(len(uris(t, h.File("me-gusta.json"))), 2)

	// The local cache is lost: the previous CARs are read back from the NAS.
	h.CacheDir = t.TempDir()
	f.mutex.Lock()
	f.records["app.bsky.feed.post/3kkkkkkkkkk22"] = post("Otro", "2025-02-03T07:00:00.000Z", nil)
	f.commit()
	f.mutex.Unlock()
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	biff.AssertEqual(f.since, []string{revName(1), revName(2)})
	biff.AssertEqual(uris(t, h.File("publicaciones/2025/2025-02.json")), []string{"3kiiiiiiiii22", "3kkkkkkkkkk22"})

	// A whole new install over the same folder: a new full CAR, nothing else.
	h.Cursor, h.Manifest, h.CacheDir = nil, map[string]string{}, t.TempDir()
	created := h.NAS.Created
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(h.NAS.Created, created+1)
	biff.AssertTrue(h.Has("repositorio/" + revName(3) + "-completo.car"))
	biff.AssertEqual(uris(t, h.File("publicaciones/2024/2024-12.json")), []string{"3kaaaaaaaaa22"})
}

func TestBlueskyRejectsTamperedRepo(t *testing.T) {
	f := newFakePDS(t)
	seed(f)
	f.tamper = true
	h := plugintest.New(t, bluesky.Plugin{}, plugin.Config{"handle": "ana.test", "password": password, "pds": f.entry.URL})
	r := h.Run()
	biff.AssertNotNil(r.Err)
	biff.AssertTrue(strings.Contains(r.Err.Error(), "huella"))
	biff.AssertFalse(h.Has("repositorio/" + revName(1) + "-completo.car"))
}

func TestBlueskyValidate(t *testing.T) {
	f := newFakePDS(t)
	h := plugintest.New(t, bluesky.Plugin{}, plugin.Config{"handle": "ana.test", "password": "otra", "pds": f.entry.URL})
	err := h.Validate()
	biff.AssertNotNil(err)
	biff.AssertTrue(strings.Contains(err.Error(), "contraseña de aplicación"))
	h.Config["password"] = ""
	biff.AssertNotNil(h.Validate())
}

func TestBlueskyBlobsGoOnWhenTheRepoDidNotChange(t *testing.T) {
	f := newFakePDS(t)
	photo, _ := seed(f)
	f.failList = 1
	h := plugintest.New(t, bluesky.Plugin{}, plugin.Config{"handle": "ana.test", "password": password, "pds": f.entry.URL})
	r := h.Run()
	biff.AssertNotNil(r.Err)
	biff.AssertTrue(strings.Contains(r.Err.Error(), "próxima ejecución"))
	biff.AssertTrue(h.Has("publicaciones/2025/2025-01.json"))
	biff.AssertFalse(h.Has("media/2025/2025-01/" + photo + ".jpg"))

	// Same revision: the CAR is not downloaded again, but the blobs are, in
	// the folder of their post (known from the CARs kept).
	h.CacheDir = t.TempDir()
	calls := f.repoCalls
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(f.repoCalls, calls)
	biff.AssertEqual(string(h.File("media/2025/2025-01/"+photo+".jpg")), "jpeg del perro")
}
