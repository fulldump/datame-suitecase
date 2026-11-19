package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fulldump/apitest"
	"github.com/fulldump/biff"

	"github.com/fulldump/datame-suitcase/engine"
	"github.com/fulldump/datame-suitcase/nastest"
	_ "github.com/fulldump/datame-suitcase/plugins/all"
	"github.com/fulldump/datame-suitcase/server"
)

func newAPI(t *testing.T, options server.Options) *apitest.Apitest {
	e, err := engine.New(engine.Options{Dir: t.TempDir()})
	biff.AssertNil(err)
	a := apitest.NewWithHandler(server.Handler(e, options))
	t.Cleanup(a.Destroy)
	return a
}

func TestAPI(t *testing.T) {
	fake := nastest.New()
	defer fake.Close()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<rss version="2.0"><channel><item><title>Hola</title><guid>1</guid><pubDate>2024-05-01</pubDate></item></channel></rss>`)
	}))
	defer feed.Close()
	a := newAPI(t, server.Options{})
	do := func(method, path string, body any) *apitest.Response {
		r := a.Request(method, path).WithHeader("X-Suitcase", "1")
		if body != nil {
			r = r.WithBodyJson(body)
		}
		return r.Do()
	}

	page := a.Request("GET", "/").Do()
	biff.AssertEqual(page.StatusCode, http.StatusOK)
	biff.AssertTrue(strings.Contains(page.BodyString(), "Datame Suitcase"))
	biff.AssertEqual(a.Request("GET", "/app.js").Do().StatusCode, http.StatusOK)

	plugins := do("GET", "/api/plugins", nil)
	biff.AssertEqual(plugins.StatusCode, http.StatusOK)
	kinds := []string{}
	for _, p := range plugins.BodyJson().([]any) {
		kinds = append(kinds, p.(map[string]any)["kind"].(string))
	}
	for _, kind := range []string{"rss", "twitter", "telegram", "github", "carpeta", "url"} {
		biff.AssertTrue(strings.Contains(strings.Join(kinds, ","), kind))
	}

	biff.AssertEqual(do("GET", "/api/nas", nil).BodyJsonMap()["paired"], false)
	bad := do("POST", "/api/nas", map[string]string{"code": "hola"})
	biff.AssertEqual(bad.StatusCode, http.StatusBadRequest)
	biff.AssertTrue(strings.Contains(bad.BodyJsonMap()["error"].(string), "Conexiones"))
	paired := do("POST", "/api/nas", map[string]string{"code": fake.Code().Encode()})
	biff.AssertEqual(paired.StatusCode, http.StatusOK)
	biff.AssertEqual(paired.BodyJsonMap()["reachable"], true)

	invalid := do("POST", "/api/integrations", map[string]any{"kind": "rss", "volume": "principal", "folder": "/Blog", "config": map[string]string{}})
	biff.AssertEqual(invalid.StatusCode, http.StatusBadRequest)
	biff.AssertTrue(strings.Contains(invalid.BodyJsonMap()["error"].(string), "Dirección del feed"))

	created := do("POST", "/api/integrations", map[string]any{"kind": "rss", "volume": "principal", "folder": "/Blog", "enabled": true, "config": map[string]string{"url": feed.URL}})
	biff.AssertEqual(created.StatusCode, http.StatusCreated)
	id := created.BodyJsonMap()["id"].(string)

	biff.AssertEqual(do("POST", "/api/integrations/"+id+"/run", nil).StatusCode, http.StatusAccepted)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var item engine.View
		biff.AssertNil(json.Unmarshal(do("GET", "/api/integrations/"+id, nil).BodyBytes(), &item))
		if item.LastEnd > 0 && !item.Running {
			biff.AssertEqual(item.LastError, "")
			biff.AssertEqual(item.LastItems, 1)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	logLines := do("GET", "/api/integrations/"+id+"/log", nil).BodyJson().([]any)
	biff.AssertTrue(len(logLines) > 0)

	updated := do("PUT", "/api/integrations/"+id, map[string]any{"name": "Mi blog", "volume": "principal", "folder": "/Blog", "every_hours": 12, "config": map[string]string{"url": feed.URL}})
	biff.AssertEqual(updated.StatusCode, http.StatusOK)
	var view engine.View
	biff.AssertNil(json.Unmarshal(updated.BodyBytes(), &view))
	biff.AssertEqual(view.EveryHours, 12)
	biff.AssertEqual(view.Name, "Mi blog")
	biff.AssertEqual(len(do("GET", "/api/integrations", nil).BodyJson().([]any)), 1)

	biff.AssertEqual(do("DELETE", "/api/integrations/"+id, nil).StatusCode, http.StatusNoContent)
	biff.AssertEqual(do("GET", "/api/integrations/"+id, nil).StatusCode, http.StatusNotFound)
	biff.AssertEqual(do("DELETE", "/api/nas", nil).StatusCode, http.StatusNoContent)
}

func TestAPIKeepsOthersOut(t *testing.T) {
	a := newAPI(t, server.Options{})
	// A change without the header, as a form of another web page would send it.
	biff.AssertEqual(a.Request("POST", "/api/nas").WithBodyJson(map[string]string{"code": "x"}).Do().StatusCode, http.StatusForbidden)
	// A name that points to this machine but is not localhost (DNS rebinding).
	biff.AssertEqual(a.Request("GET", "/api/integrations").WithHost("evil.example:4141").Do().StatusCode, http.StatusForbidden)
	biff.AssertEqual(a.Request("GET", "/api/integrations").WithHost("localhost:4141").Do().StatusCode, http.StatusOK)

	locked := newAPI(t, server.Options{Password: "abre"})
	biff.AssertEqual(locked.Request("GET", "/api/integrations").WithHost("nas.local:4141").Do().StatusCode, http.StatusUnauthorized)
	r := locked.Request("GET", "/api/integrations").WithHost("nas.local:4141")
	r.WithHeader("Authorization", "Basic "+basic("x", "abre"))
	biff.AssertEqual(r.Do().StatusCode, http.StatusOK)
}

func basic(user, password string) string {
	req, _ := http.NewRequest("GET", "/", nil)
	req.SetBasicAuth(user, password)
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Basic ")
}
