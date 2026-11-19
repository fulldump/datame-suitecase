package linkedin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/plugin"
	"github.com/fulldump/datame-suitcase/plugins/linkedin"
	"github.com/fulldump/datame-suitcase/plugintest"
)

const (
	cookie   = "AQEDAR-secreta"
	session  = "ajax:123456"
	memberID = "ACoAAAna"
)

// activity makes the urn of a post written at a given time: the time is in the
// first bits of the id.
func activity(at time.Time) string {
	return fmt.Sprintf("urn:li:activity:%d", uint64(at.UnixMilli())<<22|12345)
}

// fakeLinkedIn answers like the internal API of linkedin.com, in its
// normalized form, for the account of Ana.
type fakeLinkedIn struct {
	mutex    sync.Mutex
	requests []string
	broken   map[string]int // path prefix → status to answer
	messages []map[string]any
}

func normalizedJSON(data map[string]any, included ...map[string]any) []byte {
	out, _ := json.Marshal(map[string]any{"data": data, "included": included})
	return out
}

func (f *fakeLinkedIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mutex.Lock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	broken := f.broken
	messages := f.messages
	f.mutex.Unlock()

	if !strings.Contains(r.Header.Get("Cookie"), "li_at="+cookie) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if r.URL.Path == "/uas/authenticate" {
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: `"` + session + `"`})
		return
	}
	if r.Header.Get("Csrf-Token") != session {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	for prefix, status := range broken {
		if strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(status)
			return
		}
	}
	mini := map[string]any{"$type": "com.linkedin.voyager.identity.shared.MiniProfile", "entityUrn": "urn:li:fs_miniProfile:" + memberID,
		"firstName": "Ana", "lastName": "Pérez", "publicIdentifier": "ana-perez"}
	miniBea := map[string]any{"$type": "com.linkedin.voyager.identity.shared.MiniProfile", "entityUrn": "urn:li:fs_miniProfile:ACoBea",
		"firstName": "Bea", "lastName": "Ruiz", "publicIdentifier": "bea-ruiz"}
	switch {
	case r.URL.Path == "/voyager/api/me":
		w.Write(normalizedJSON(map[string]any{"*miniProfile": mini["entityUrn"]}, mini))
	case strings.HasSuffix(r.URL.Path, "/profileView"):
		w.Write(normalizedJSON(map[string]any{},
			map[string]any{"$type": "com.linkedin.voyager.identity.profile.Profile", "entityUrn": "urn:li:fs_profile:" + memberID, "publicIdentifier": "ana-perez",
				"firstName": "Ana", "lastName": "Pérez", "headline": "Ingeniera de datos", "summary": "Me gusta ordenar cosas.", "locationName": "Sevilla, España", "industryName": "Software"},
			map[string]any{"$type": "com.linkedin.voyager.identity.profile.Position", "entityUrn": "urn:li:fs_position:1", "title": "Ingeniera de datos", "companyName": "Ejemplo S.L.",
				"timePeriod": map[string]any{"startDate": map[string]any{"month": 3, "year": 2024}}, "description": "Pipelines y copias."},
			map[string]any{"$type": "com.linkedin.voyager.identity.profile.Education", "entityUrn": "urn:li:fs_education:1", "schoolName": "Universidad de Ejemplo", "degreeName": "Grado",
				"fieldOfStudy": "Informática", "timePeriod": map[string]any{"startDate": map[string]any{"year": 2018}, "endDate": map[string]any{"year": 2022}}},
			map[string]any{"$type": "com.linkedin.voyager.identity.profile.Skill", "entityUrn": "urn:li:fs_skill:1", "name": "Go"},
		))
	case r.URL.Path == "/voyager/api/relationships/dash/connections":
		w.Write(normalizedJSON(map[string]any{},
			map[string]any{"$type": "com.linkedin.voyager.dash.relationships.Connection", "entityUrn": "urn:li:fsd_connection:1", "createdAt": time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC).UnixMilli(),
				"*connectedMemberResolutionResult": "urn:li:fsd_profile:ACoBea"},
			map[string]any{"$type": "com.linkedin.voyager.dash.identity.profile.Profile", "entityUrn": "urn:li:fsd_profile:ACoBea", "firstName": "Bea", "lastName": "Ruiz",
				"publicIdentifier": "bea-ruiz", "headline": "Diseñadora"},
		))
	case r.URL.Path == "/voyager/api/messaging/conversations":
		last := int64(0)
		for _, m := range messages {
			if at := m["createdAt"].(int64); at > last {
				last = at
			}
		}
		w.Write(normalizedJSON(map[string]any{"*elements": []string{"urn:li:fs_conversation:2-c1"}},
			map[string]any{"$type": "com.linkedin.voyager.messaging.Conversation", "entityUrn": "urn:li:fs_conversation:2-c1", "lastActivityAt": last,
				"*participants": []string{"urn:li:fs_messagingMember:bea"}},
			map[string]any{"$type": "com.linkedin.voyager.messaging.MessagingMember", "entityUrn": "urn:li:fs_messagingMember:bea", "*miniProfile": miniBea["entityUrn"]},
			miniBea,
		))
	case r.URL.Path == "/voyager/api/messaging/conversations/2-c1/events":
		included := []map[string]any{miniBea, mini,
			{"$type": "com.linkedin.voyager.messaging.MessagingMember", "entityUrn": "urn:li:fs_messagingMember:bea", "*miniProfile": miniBea["entityUrn"]},
			{"$type": "com.linkedin.voyager.messaging.MessagingMember", "entityUrn": "urn:li:fs_messagingMember:ana", "*miniProfile": mini["entityUrn"]}}
		elements := []string{}
		for i, m := range messages {
			urn := fmt.Sprintf("urn:li:fs_event:%d", i)
			elements = append(elements, urn)
			included = append(included, map[string]any{"$type": "com.linkedin.voyager.messaging.Event", "entityUrn": urn, "createdAt": m["createdAt"],
				"*from":        "urn:li:fs_messagingMember:" + m["from"].(string),
				"eventContent": map[string]any{"com.linkedin.voyager.messaging.event.MessageEvent": map[string]any{"attributedBody": map[string]any{"text": m["text"]}}}})
		}
		w.Write(normalizedJSON(map[string]any{"*elements": elements}, included...))
	case r.URL.Path == "/voyager/api/identity/profileUpdatesV2":
		w.Write(normalizedJSON(map[string]any{},
			map[string]any{"$type": "com.linkedin.voyager.feed.render.UpdateV2", "entityUrn": "urn:li:fs_updateV2:1",
				"updateMetadata": map[string]any{"urn": activity(time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))},
				"actor":          map[string]any{"urn": "urn:li:member:" + memberID}, "commentary": map[string]any{"text": map[string]any{"text": "Hoy he aprendido a guardar mis datos."}}},
			map[string]any{"$type": "com.linkedin.voyager.feed.render.UpdateV2", "entityUrn": "urn:li:fs_updateV2:2",
				"updateMetadata": map[string]any{"urn": activity(time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC))},
				"actor":          map[string]any{"urn": "urn:li:member:someone-else"}, "commentary": map[string]any{"text": map[string]any{"text": "No es mío."}}},
		))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeLinkedIn) count(prefix string) int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func newFake() *fakeLinkedIn {
	return &fakeLinkedIn{broken: map[string]int{}, messages: []map[string]any{
		{"createdAt": time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC).UnixMilli(), "from": "bea", "text": "¡Hola, Ana!\nGracias por aceptar."},
	}}
}

func TestLinkedInCopiesOnItsOwn(t *testing.T) {
	fake := newFake()
	server := httptest.NewServer(fake)
	defer server.Close()
	h := plugintest.New(t, linkedin.Plugin{}, plugin.Config{"li_at": cookie, "base": server.URL})
	biff.AssertNil(h.Validate())

	r := h.Run()
	biff.AssertNil(r.Err)
	profile := string(h.File("perfil.md"))
	biff.AssertTrue(strings.Contains(profile, "# Ana Pérez"))
	biff.AssertTrue(strings.Contains(profile, "### Ingeniera de datos — Ejemplo S.L."))
	biff.AssertTrue(strings.Contains(profile, "Mar 2024 – actualidad"))
	biff.AssertTrue(strings.Contains(profile, "Grado, Informática"))
	biff.AssertTrue(strings.Contains(string(h.File("contactos.csv")), "https://www.linkedin.com/in/bea-ruiz"))
	biff.AssertTrue(h.Has("api/perfil.json"))

	messages := string(h.File(filepath.ToSlash("mensajes/bea-ruiz-" + plugin.ShortHash("2-c1")[:6] + "/2026/2026-11.md")))
	biff.AssertTrue(strings.Contains(messages, "Gracias por aceptar."))
	posts := string(h.File("publicaciones/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(posts, "Hoy he aprendido a guardar mis datos."))
	biff.AssertFalse(strings.Contains(posts, "No es mío."))

	// Nothing new: the conversation did not change, its messages are not read again.
	events := fake.count("/voyager/api/messaging/conversations/2-c1/events")
	puts := h.NAS.Puts
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(fake.count("/voyager/api/messaging/conversations/2-c1/events"), events)
	biff.AssertEqual(h.NAS.Puts, puts)

	// A new message: only it is added.
	fake.mutex.Lock()
	fake.messages = append(fake.messages, map[string]any{"createdAt": time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC).UnixMilli(), "from": "ana", "text": "¡De nada!"})
	fake.mutex.Unlock()
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 1)
	messages = string(h.File("mensajes/bea-ruiz-" + plugin.ShortHash("2-c1")[:6] + "/2026/2026-11.md"))
	biff.AssertTrue(strings.Contains(messages, "¡De nada!"))
	biff.AssertEqual(strings.Count(messages, "Gracias por aceptar."), 1)
}

func TestLinkedInWithAnExpiredCookieSaysWhatToDo(t *testing.T) {
	server := httptest.NewServer(newFake())
	defer server.Close()
	h := plugintest.New(t, linkedin.Plugin{}, plugin.Config{"li_at": "caducada", "base": server.URL})
	err := h.Validate()
	biff.AssertNotNil(err)
	biff.AssertTrue(strings.Contains(err.Error(), "caducado"))
	r := h.Run()
	biff.AssertNotNil(r.Err)
	biff.AssertTrue(strings.Contains(r.Err.Error(), "li_at"))
}

func TestLinkedInStopsWhenItIsLimited(t *testing.T) {
	fake := newFake()
	fake.broken["/voyager/api/relationships"] = 999
	server := httptest.NewServer(fake)
	defer server.Close()
	h := plugintest.New(t, linkedin.Plugin{}, plugin.Config{"li_at": cookie, "base": server.URL})
	r := h.Run()
	biff.AssertNotNil(r.Err)
	biff.AssertTrue(strings.Contains(r.Err.Error(), "limitando"))
	biff.AssertTrue(h.Has("perfil.md"))                                       // what came before is kept
	biff.AssertEqual(fake.count("/voyager/api/messaging"), 0)                 // and nothing more is asked
	biff.AssertEqual(fake.count("/voyager/api/identity/profileUpdatesV2"), 0) // not even the posts
}

func TestLinkedInGoesOnWhenOnePartChanged(t *testing.T) {
	fake := newFake()
	fake.broken["/voyager/api/messaging"] = http.StatusGone // LinkedIn moved its messages
	server := httptest.NewServer(fake)
	defer server.Close()
	h := plugintest.New(t, linkedin.Plugin{}, plugin.Config{"li_at": cookie, "base": server.URL})
	r := h.Run()
	biff.AssertNil(r.Err)
	biff.AssertTrue(h.Has("perfil.md"))
	biff.AssertTrue(h.Has("contactos.csv"))
	biff.AssertTrue(h.Has("publicaciones/2026/2026-11.json"))
	logged := false
	for _, line := range h.Log {
		logged = logged || strings.Contains(line, "No se han podido copiar")
	}
	biff.AssertTrue(logged)
}

func TestLinkedInLiveAndExportMerge(t *testing.T) {
	fake := newFake()
	server := httptest.NewServer(fake)
	defer server.Close()
	dir := t.TempDir()
	writeZip(t, filepath.Join(dir, "Complete_LinkedInDataExport_11-01-2026.zip"), export([]string{bea, carlos}, nil, nil), time.Hour)
	h := plugintest.New(t, linkedin.Plugin{}, plugin.Config{"li_at": cookie, "base": server.URL, "path": dir})
	r := h.Run()
	biff.AssertNil(r.Err)
	// Bea comes from both ways and is there once; Carlos, only in the export, stays.
	contacts := string(h.File("contactos.csv"))
	biff.AssertEqual(strings.Count(contacts, "bea-ruiz"), 1)
	biff.AssertTrue(strings.Contains(contacts, "carlos-gil"))

	// The cursor of the importer survives the live copy, and the other way round.
	r = h.Run()
	biff.AssertNil(r.Err)
	biff.AssertEqual(r.Items, 0)
	biff.AssertEqual(fake.count("/voyager/api/messaging/conversations/2-c1/events"), 1)
}
